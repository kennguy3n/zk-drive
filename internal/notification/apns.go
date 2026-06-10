package notification

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/net/http2"
)

const (
	// apnsProdHost / apnsSandboxHost are the APNs HTTP/2 provider API
	// hosts. Sandbox receives pushes for development builds (apps signed
	// with a development provisioning profile); production for TestFlight
	// / App Store builds.
	apnsProdHost    = "https://api.push.apple.com"
	apnsSandboxHost = "https://api.sandbox.push.apple.com"
	// apnsTokenTTL is how long a provider authentication token is reused.
	// Apple mandates refreshing it no more often than every 20 minutes and
	// no less often than every 60; 40 minutes sits safely in the middle so
	// we neither get throttled (TooManyProviderTokenUpdates) nor rejected
	// (ExpiredProviderToken).
	apnsTokenTTL = 40 * time.Minute
)

// APNsProvider delivers notifications to iOS devices through the APNs
// HTTP/2 provider API using token-based authentication (a .p8 ES256
// signing key + key id + team id). The provider JWT is signed once and
// reused across requests until apnsTokenTTL elapses.
//
// A nil *APNsProvider is a no-op (Send returns delivered=false, err=nil).
type APNsProvider struct {
	teamID  string
	keyID   string
	topic   string // the app's bundle identifier
	signKey *ecdsa.PrivateKey
	host    string
	httpClient httpDoer

	now func() time.Time

	// mu guards the cached provider token and coalesces concurrent
	// re-signs so a burst of sends signs at most one token.
	mu          sync.Mutex
	token       string
	tokenIssued time.Time
}

// NewAPNsProvider builds a provider from the raw .p8 key contents plus
// the key id, team id and topic (the app's bundle identifier).
// production selects the prod vs sandbox APNs host. Returns an error if
// any field is empty or the key cannot be parsed, so the server leaves
// APNs disabled rather than starting half-configured.
func NewAPNsProvider(p8 []byte, keyID, teamID, topic string, production bool) (*APNsProvider, error) {
	if keyID == "" || teamID == "" || topic == "" {
		return nil, errors.New("apns: key id, team id and topic are required")
	}
	key, err := parseECPrivateKey(p8)
	if err != nil {
		return nil, fmt.Errorf("apns: %w", err)
	}
	host := apnsSandboxHost
	if production {
		host = apnsProdHost
	}
	return &APNsProvider{
		teamID:     teamID,
		keyID:      keyID,
		topic:      topic,
		signKey:    key,
		host:       host,
		httpClient: newHTTP2Client(),
		now:        time.Now,
	}, nil
}

// WithHTTPClient overrides the HTTP client (tests inject a stub). Fluent.
func (p *APNsProvider) WithHTTPClient(c httpDoer) *APNsProvider {
	if p != nil && c != nil {
		p.httpClient = c
	}
	return p
}

// Platform returns PlatformIOS: APNs serves iOS device tokens.
func (p *APNsProvider) Platform() Platform { return PlatformIOS }

// newHTTP2Client returns an HTTP client pinned to HTTP/2, which APNs
// requires (it refuses HTTP/1.1). The standard transport negotiates h2
// over TLS via ALPN, but ConfigureTransport makes the requirement
// explicit and enables prior-knowledge h2 so a misconfigured proxy
// cannot silently downgrade us to a rejected HTTP/1.1 request.
func newHTTP2Client() *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	// ConfigureTransport only errors if the transport already has TLSNextProto
	// populated; a fresh transport never does, so this cannot fail here.
	_ = http2.ConfigureTransport(tr)
	return &http.Client{Transport: tr, Timeout: providerHTTPTimeout}
}

// parseECPrivateKey decodes a PEM-encoded PKCS#8 EC private key — the
// format Apple issues .p8 auth keys in.
func parseECPrivateKey(p8 []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(p8)
	if block == nil {
		return nil, errors.New("auth key is not valid PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse auth key: %w", err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("auth key is %T, want ECDSA", key)
	}
	return ecKey, nil
}

// apnsErrorResponse models the APNs error body: a single `reason` string.
type apnsErrorResponse struct {
	Reason string `json:"reason"`
}

// Send delivers payload to one APNs device token. See MobilePushProvider
// for the (dead, err) contract.
func (p *APNsProvider) Send(ctx context.Context, token string, payload NotificationPayload) (bool, error) {
	if p == nil {
		return false, nil
	}
	authToken, err := p.providerToken()
	if err != nil {
		return false, fmt.Errorf("apns: sign provider token: %w", err)
	}

	body, err := buildAPNsPayload(payload)
	if err != nil {
		return false, fmt.Errorf("apns: marshal payload: %w", err)
	}
	// Percent-encode the token as a single path segment. Real APNs tokens
	// are hex (URL-safe), but escaping keeps a token containing a reserved
	// byte (?, #, %) from being reinterpreted as a query/fragment/escape by
	// the URL parser instead of reaching APNs verbatim.
	sendURL := p.host + "/3/device/" + url.PathEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sendURL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("apns: build request: %w", err)
	}
	req.Header.Set("authorization", "bearer "+authToken)
	req.Header.Set("apns-topic", p.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("apns: send: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode == http.StatusOK {
		return false, nil
	}

	var apnsErr apnsErrorResponse
	_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&apnsErr)

	switch {
	// 410 Unregistered (always) and the device-token-specific 400 reasons
	// are permanent: the token will never deliver, so prune it.
	case resp.StatusCode == http.StatusGone,
		apnsErr.Reason == "Unregistered",
		apnsErr.Reason == "BadDeviceToken",
		apnsErr.Reason == "DeviceTokenNotForTopic":
		return true, nil
	// ExpiredProviderToken: our cached JWT aged out (clock skew / restart
	// edge). Drop it so the next send re-signs; the token is fine.
	case apnsErr.Reason == "ExpiredProviderToken":
		p.invalidateToken()
		return false, &httpStatusErr{provider: "apns", status: resp.StatusCode, detail: apnsErr.Reason}
	default:
		return false, &httpStatusErr{provider: "apns", status: resp.StatusCode, detail: apnsErr.Reason}
	}
}

// buildAPNsPayload renders the APNs JSON: the reserved `aps` dictionary
// (the visible alert) plus any deep-link / dedupe metadata as top-level
// custom keys, mirroring the FCM `data` map.
func buildAPNsPayload(payload NotificationPayload) ([]byte, error) {
	doc := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": payload.Title,
				"body":  payload.Body,
			},
			"sound": "default",
		},
	}
	for k, v := range payloadData(payload) {
		doc[k] = v
	}
	return json.Marshal(doc)
}

// providerToken returns the cached APNs provider JWT, re-signing it when
// none is held or it has aged past apnsTokenTTL. The mutex is held across
// the (cheap, in-process) signing so concurrent sends share one token.
func (p *APNsProvider) providerToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && p.now().Before(p.tokenIssued.Add(apnsTokenTTL)) {
		return p.token, nil
	}
	now := p.now()
	claims := jwt.MapClaims{
		"iss": p.teamID,
		"iat": now.Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = p.keyID
	signed, err := tok.SignedString(p.signKey)
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	p.token = signed
	p.tokenIssued = now
	return p.token, nil
}

// invalidateToken drops the cached provider token so the next send re-signs.
func (p *APNsProvider) invalidateToken() {
	p.mu.Lock()
	p.token = ""
	p.tokenIssued = time.Time{}
	p.mu.Unlock()
}
