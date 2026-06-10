package iamcore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wellKnownPath is the OIDC discovery document path appended to the
// issuer URL per OpenID Connect Discovery 1.0.
const wellKnownPath = "/.well-known/openid-configuration"

// DiscoveryDocument is the subset of the OIDC discovery metadata
// zk-drive consumes. Fetched once at server start; the endpoints it
// advertises drive the authorize redirect, token exchange, and JWKS
// verification.
type DiscoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// TokenResponse is the RFC 6749 token-endpoint response. ExpiresIn is
// seconds-to-expiry of the access token; the SPA uses it to schedule
// silent refresh before expiry.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
}

// Client is an OAuth2/OIDC client for iam-core implementing the
// Authorization Code + PKCE flow. It is safe for concurrent use.
type Client struct {
	cfg   Config
	disco DiscoveryDocument
	http  *http.Client
}

// NewClient performs OIDC discovery against cfg.IssuerURL and returns
// a configured Client. The discovered issuer MUST equal the configured
// issuer (a standard OIDC defense against discovery-document spoofing),
// and the authorization, token, and JWKS endpoints must be present and
// absolute. httpClient may be nil.
func NewClient(ctx context.Context, cfg Config, httpClient *http.Client) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled() {
		return nil, fmt.Errorf("iamcore: NewClient called with iam-core disabled")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	disco, err := fetchDiscovery(ctx, httpClient, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	if normalizeIssuer(disco.Issuer) != normalizeIssuer(cfg.IssuerURL) {
		return nil, fmt.Errorf("iamcore: discovery issuer %q does not match configured issuer %q", disco.Issuer, cfg.IssuerURL)
	}
	for name, endpoint := range map[string]string{
		"authorization_endpoint": disco.AuthorizationEndpoint,
		"token_endpoint":         disco.TokenEndpoint,
		"jwks_uri":               disco.JWKSURI,
	} {
		if err := validateAbsoluteURL(name, endpoint); err != nil {
			return nil, fmt.Errorf("iamcore: discovery %w", err)
		}
	}
	return &Client{cfg: cfg, disco: disco, http: httpClient}, nil
}

func fetchDiscovery(ctx context.Context, httpClient *http.Client, issuer string) (DiscoveryDocument, error) {
	endpoint := normalizeIssuer(issuer) + wellKnownPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return DiscoveryDocument{}, fmt.Errorf("iamcore: build discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return DiscoveryDocument{}, fmt.Errorf("iamcore: fetch discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return DiscoveryDocument{}, fmt.Errorf("iamcore: discovery endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DiscoveryDocument{}, fmt.Errorf("iamcore: read discovery: %w", err)
	}
	var doc DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return DiscoveryDocument{}, fmt.Errorf("iamcore: decode discovery: %w", err)
	}
	return doc, nil
}

// Discovery returns the discovered OIDC metadata.
func (c *Client) Discovery() DiscoveryDocument { return c.disco }

// Config returns the client's configuration.
func (c *Client) Config() Config { return c.cfg }

// Scopes returns the effective OAuth2 scopes the client requests
// (always including "openid"). The SPA echoes these on the authorize
// redirect it builds from GET /api/config.
func (c *Client) Scopes() []string { return c.cfg.EffectiveScopes() }

// NewVerifier returns a token Verifier wired to the discovered JWKS
// endpoint and the configured issuer/audience. The server holds one
// Verifier for the lifetime of the process.
func (c *Client) NewVerifier() *Verifier {
	return NewVerifier(c.disco.Issuer, c.cfg.Audience, c.disco.JWKSURI, c.http)
}

// AuthorizeURL builds the Authorization Code + PKCE authorize redirect.
// state is the anti-CSRF / anti-fixation nonce echoed back on the
// callback; codeChallenge is the S256 challenge derived from the
// verifier the caller retains. The audience is requested via the
// `audience` parameter when configured so iam-core mints a token bound
// to this relying party.
func (c *Client) AuthorizeURL(state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.CallbackURL)
	q.Set("scope", strings.Join(c.cfg.EffectiveScopes(), " "))
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	if c.cfg.Audience != "" {
		q.Set("audience", c.cfg.Audience)
	}
	sep := "?"
	if strings.Contains(c.disco.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return c.disco.AuthorizationEndpoint + sep + q.Encode()
}

// Exchange swaps an authorization code for tokens, completing the PKCE
// flow with the original code_verifier. A configured client secret is
// sent via HTTP Basic auth (client_secret_basic); a public client
// omits it and relies on PKCE alone.
func (c *Client) Exchange(ctx context.Context, code, codeVerifier string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.CallbackURL)
	form.Set("client_id", c.cfg.ClientID)
	form.Set("code_verifier", codeVerifier)
	return c.tokenRequest(ctx, form)
}

// Refresh exchanges a refresh token for a fresh access token (and,
// typically, a rotated refresh token). Used by the silent-refresh
// endpoint so the SPA never has to bounce the user through Universal
// Login while their session is alive.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.cfg.ClientID)
	if len(c.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(c.cfg.EffectiveScopes(), " "))
	}
	return c.tokenRequest(ctx, form)
}

func (c *Client) tokenRequest(ctx context.Context, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.disco.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("iamcore: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.cfg.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("iamcore: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("iamcore: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("iamcore: token endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("iamcore: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("iamcore: token response missing access_token")
	}
	return &tr, nil
}

// NewPKCE returns a fresh (verifier, S256 challenge) pair per RFC 7636.
// The verifier is a 43-character base64url string (32 random bytes),
// the minimum length the spec recommends.
func NewPKCE() (verifier, challenge string, err error) {
	verifier, err = randomURLSafe(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// NewState returns a cryptographically random state value for CSRF
// protection on the authorize round-trip.
func NewState() (string, error) {
	return randomURLSafe(32)
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("iamcore: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
