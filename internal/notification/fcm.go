package notification

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// fcmScope is the OAuth2 scope a service account needs to call the
	// FCM HTTP v1 send endpoint.
	fcmScope = "https://www.googleapis.com/auth/firebase.messaging"
	// defaultGoogleTokenURI is the OAuth2 token endpoint used when the
	// service-account JSON omits token_uri (it normally includes it).
	defaultGoogleTokenURI = "https://oauth2.googleapis.com/token"
	// fcmSendEndpointFmt is the HTTP v1 send endpoint; %s is the project id.
	fcmSendEndpointFmt = "https://fcm.googleapis.com/v1/projects/%s/messages:send"
	// jwtBearerGrant is the assertion grant type for service-account auth.
	jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// tokenRefreshSkew refreshes the cached OAuth2 access token this long
	// before it actually expires so an in-flight send never races expiry.
	tokenRefreshSkew = 60 * time.Second
	// providerHTTPTimeout bounds a single call to a push provider (token
	// mint or send). Delivery already runs in the publisher's detached,
	// pushDeliveryTimeout-bounded goroutine; this is the per-request floor.
	providerHTTPTimeout = 10 * time.Second
)

// serviceAccount is the subset of a Google service-account JSON key the
// FCM provider needs to mint OAuth2 access tokens.
type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// FCMProvider delivers notifications to Android devices through the FCM
// HTTP v1 API, authenticating with a Google service account. It mints
// and caches a short-lived OAuth2 access token from the service-account
// key (RS256-signed JWT exchanged at the Google token endpoint),
// refreshing it transparently before expiry.
//
// A nil *FCMProvider is a no-op (Send returns delivered=false, err=nil)
// so a caller that wraps an unconfigured provider in the interface still
// behaves; in practice the server only registers a non-nil provider.
type FCMProvider struct {
	projectID   string
	clientEmail string
	tokenURI    string
	privateKey  *rsa.PrivateKey
	sendURL     string
	httpClient  httpDoer

	// now is time.Now in production, overridable in tests to exercise the
	// token cache without sleeping.
	now func() time.Time

	// mu guards the cached access token and coalesces concurrent refreshes
	// so a burst of sends mints at most one token.
	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// NewFCMProvider builds a provider from the raw service-account JSON
// (the file Google hands you when you create a service-account key).
// Returns an error if the JSON is missing required fields or the private
// key cannot be parsed — the server logs it and leaves FCM disabled
// rather than starting with a half-configured provider.
func NewFCMProvider(serviceAccountJSON []byte) (*FCMProvider, error) {
	var sa serviceAccount
	if err := json.Unmarshal(serviceAccountJSON, &sa); err != nil {
		return nil, fmt.Errorf("fcm: parse service account json: %w", err)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("fcm: service account json missing project_id, client_email or private_key")
	}
	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("fcm: %w", err)
	}
	tokenURI := strings.TrimSpace(sa.TokenURI)
	if tokenURI == "" {
		tokenURI = defaultGoogleTokenURI
	}
	return &FCMProvider{
		projectID:   sa.ProjectID,
		clientEmail: sa.ClientEmail,
		tokenURI:    tokenURI,
		privateKey:  key,
		sendURL:     fmt.Sprintf(fcmSendEndpointFmt, sa.ProjectID),
		httpClient:  &http.Client{Timeout: providerHTTPTimeout},
		now:         time.Now,
	}, nil
}

// WithHTTPClient overrides the HTTP client (used by tests to inject a
// stub that returns canned token / send responses). Fluent.
func (p *FCMProvider) WithHTTPClient(c httpDoer) *FCMProvider {
	if p != nil && c != nil {
		p.httpClient = c
	}
	return p
}

// Platform returns PlatformAndroid: FCM serves Android device tokens.
func (p *FCMProvider) Platform() Platform { return PlatformAndroid }

// parseRSAPrivateKey decodes a PEM-encoded RSA private key. Google
// service-account keys are PKCS#8 ("PRIVATE KEY"); the PKCS#1
// ("RSA PRIVATE KEY") fallback covers keys that were re-encoded.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is %T, want RSA", key)
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return key, nil
}

// fcmSendRequest is the HTTP v1 send body. Only the fields we populate
// are modelled.
type fcmSendRequest struct {
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token        string            `json:"token"`
	Notification fcmNotification   `json:"notification"`
	Data         map[string]string `json:"data,omitempty"`
	Android      *fcmAndroidConfig `json:"android,omitempty"`
}

type fcmNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type fcmAndroidConfig struct {
	// Collapse coalesces a redundant re-delivery of the same notification
	// on the device, matching the per-notification Tag used for Web Push.
	CollapseKey string `json:"collapse_key,omitempty"`
	Priority    string `json:"priority,omitempty"`
}

// fcmErrorEnvelope models the v1 error response so we can read the
// stable errorCode in details rather than string-matching the message.
type fcmErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Details []struct {
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

// Send delivers payload to one FCM token. See MobilePushProvider for the
// (dead, err) contract.
func (p *FCMProvider) Send(ctx context.Context, token string, payload NotificationPayload) (bool, error) {
	if p == nil {
		return false, nil
	}
	accessToken, err := p.accessTokenValue(ctx)
	if err != nil {
		return false, fmt.Errorf("fcm: mint access token: %w", err)
	}

	reqBody := fcmSendRequest{Message: fcmMessage{
		Token:        token,
		Notification: fcmNotification{Title: payload.Title, Body: payload.Body},
		Data:         payloadData(payload),
		Android:      &fcmAndroidConfig{CollapseKey: payload.Tag, Priority: "high"},
	}}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return false, fmt.Errorf("fcm: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.sendURL, bytes.NewReader(raw))
	if err != nil {
		return false, fmt.Errorf("fcm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("fcm: send: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode == http.StatusOK {
		return false, nil
	}

	// Read the stable errorCode from the v1 error envelope (best-effort:
	// a body we cannot parse just yields an empty code).
	var env fcmErrorEnvelope
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<10))
	_ = dec.Decode(&env)
	code := ""
	for _, d := range env.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
			break
		}
	}

	switch {
	// UNREGISTERED (token expired / app uninstalled) and
	// SENDER_ID_MISMATCH (token belongs to another Firebase project) are
	// permanent for this sender — prune the token.
	case code == "UNREGISTERED" || code == "SENDER_ID_MISMATCH" || resp.StatusCode == http.StatusNotFound:
		return true, nil
	// 401 means our cached access token is stale/revoked; drop it so the
	// next send re-mints. The token itself is fine — transient.
	case resp.StatusCode == http.StatusUnauthorized:
		p.invalidateToken()
		return false, &httpStatusErr{provider: "fcm", status: resp.StatusCode, detail: env.Error.Status}
	default:
		// INVALID_ARGUMENT et al: could be a payload problem, not a dead
		// token, so do NOT prune — surface as transient.
		return false, &httpStatusErr{provider: "fcm", status: resp.StatusCode, detail: env.Error.Status}
	}
}

// accessTokenValue returns a cached OAuth2 access token, minting a fresh
// one (and caching it) when none is held or the current one is within
// tokenRefreshSkew of expiry. The mutex is held across the network mint
// so a burst of concurrent sends performs exactly one token exchange.
func (p *FCMProvider) accessTokenValue(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accessToken != "" && p.now().Before(p.tokenExpiry.Add(-tokenRefreshSkew)) {
		return p.accessToken, nil
	}
	assertion, err := p.signAssertion()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {jwtBearerGrant},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange: unexpected status %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("token exchange: empty access_token")
	}
	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	p.accessToken = tr.AccessToken
	p.tokenExpiry = p.now().Add(time.Duration(expiresIn) * time.Second)
	return p.accessToken, nil
}

// signAssertion builds and RS256-signs the service-account JWT that is
// exchanged for an access token.
func (p *FCMProvider) signAssertion() (string, error) {
	now := p.now()
	claims := jwt.MapClaims{
		"iss":   p.clientEmail,
		"scope": fcmScope,
		"aud":   p.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(p.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign assertion: %w", err)
	}
	return signed, nil
}

// invalidateToken drops the cached access token so the next send re-mints.
func (p *FCMProvider) invalidateToken() {
	p.mu.Lock()
	p.accessToken = ""
	p.tokenExpiry = time.Time{}
	p.mu.Unlock()
}
