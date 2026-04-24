package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// OAuthConfig bundles the subset of internal/config.Config fields
// OAuthHandler needs. Expressed as its own type so the auth package
// does not pull the whole runtime config struct into its test
// surface.
type OAuthConfig struct {
	GoogleClientID        string
	GoogleClientSecret    string
	GoogleRedirectURL     string
	MicrosoftClientID     string
	MicrosoftClientSecret string
	MicrosoftRedirectURL  string
}

// Provider values match the auth_provider column on users.
const (
	ProviderGoogle    = "google"
	ProviderMicrosoft = "microsoft"
)

// stateCookieName carries the signed state + PKCE verifier back to
// the callback. Short TTL (5 min) keeps the window tight.
const stateCookieName = "zkdrive_oauth_state"
const stateCookieTTL = 5 * time.Minute

// googleEndpoint and microsoftEndpoint are provided inline to avoid
// pulling the provider-specific x/oauth2 subpackages into go.sum just
// for a pair of URL constants. These match the common endpoints used
// by the google and microsoft (v2.0 common tenant) flows.
var googleEndpoint = oauth2.Endpoint{
	AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
}

var microsoftEndpoint = oauth2.Endpoint{
	AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
	TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
}

// OAuthHandler implements the two SSO round-trips per provider. It
// reuses the zk-drive Handler's user service + JWT secret to complete
// login once a provider returns an email. The handler gates calls on
// whether a client id is configured for the provider; missing client
// ids produce 501 Not Implemented so a dev server without SSO
// credentials still responds cleanly.
type OAuthHandler struct {
	auth   *Handler
	cfg    OAuthConfig
	audit  *audit.Service
	google *oauth2.Config
	ms     *oauth2.Config
}

// NewOAuthHandler returns a handler that composes onto the existing
// auth.Handler. The auth.Handler must be fully wired (users service,
// workspace service, jwt secret) before NewOAuthHandler is invoked.
func NewOAuthHandler(a *Handler, cfg OAuthConfig) *OAuthHandler {
	h := &OAuthHandler{auth: a, cfg: cfg}
	if cfg.GoogleClientID != "" {
		h.google = &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.GoogleRedirectURL,
			Endpoint:     googleEndpoint,
			Scopes:       []string{"openid", "email", "profile"},
		}
	}
	if cfg.MicrosoftClientID != "" {
		h.ms = &oauth2.Config{
			ClientID:     cfg.MicrosoftClientID,
			ClientSecret: cfg.MicrosoftClientSecret,
			RedirectURL:  cfg.MicrosoftRedirectURL,
			Endpoint:     microsoftEndpoint,
			Scopes:       []string{"openid", "email", "profile", "User.Read"},
		}
	}
	return h
}

// WithAudit wires an audit service so SSO login events are recorded.
func (h *OAuthHandler) WithAudit(svc *audit.Service) *OAuthHandler {
	h.audit = svc
	return h
}

// StartGoogle redirects the caller to Google's auth endpoint.
func (h *OAuthHandler) StartGoogle(w http.ResponseWriter, r *http.Request) {
	h.start(w, r, h.google, ProviderGoogle)
}

// CallbackGoogle exchanges the returned code and completes the login.
func (h *OAuthHandler) CallbackGoogle(w http.ResponseWriter, r *http.Request) {
	h.callback(w, r, h.google, ProviderGoogle)
}

// StartMicrosoft redirects the caller to Microsoft Entra's auth endpoint.
func (h *OAuthHandler) StartMicrosoft(w http.ResponseWriter, r *http.Request) {
	h.start(w, r, h.ms, ProviderMicrosoft)
}

// CallbackMicrosoft exchanges the returned code and completes the login.
func (h *OAuthHandler) CallbackMicrosoft(w http.ResponseWriter, r *http.Request) {
	h.callback(w, r, h.ms, ProviderMicrosoft)
}

// RegisterRoutes wires GET /{provider} and GET /{provider}/callback
// under the supplied chi router. Callers typically scope the router
// to /api/auth/oauth.
func (h *OAuthHandler) RegisterRoutes(r chi.Router) {
	r.Get("/google", h.StartGoogle)
	r.Get("/google/callback", h.CallbackGoogle)
	r.Get("/microsoft", h.StartMicrosoft)
	r.Get("/microsoft/callback", h.CallbackMicrosoft)
}

func (h *OAuthHandler) start(w http.ResponseWriter, r *http.Request, c *oauth2.Config, provider string) {
	if c == nil {
		http.Error(w, provider+" SSO not configured", http.StatusNotImplemented)
		return
	}
	state, err := randomString(32)
	if err != nil {
		http.Error(w, "generate state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	verifier, err := randomString(48)
	if err != nil {
		http.Error(w, "generate pkce: "+err.Error(), http.StatusInternalServerError)
		return
	}
	challenge := pkceChallenge(verifier)
	cookieValue := state + ":" + verifier
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    cookieValue,
		Path:     "/api/auth/oauth",
		MaxAge:   int(stateCookieTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	authURL := c.AuthCodeURL(state,
		oauth2.AccessTypeOnline,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (h *OAuthHandler) callback(w http.ResponseWriter, r *http.Request, c *oauth2.Config, provider string) {
	if c == nil {
		http.Error(w, provider+" SSO not configured", http.StatusNotImplemented)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	if state == "" || code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
		return
	}
	cookie, err := r.Cookie(stateCookieName)
	if err != nil {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(cookie.Value, ":", 2)
	if len(parts) != 2 || parts[0] != state {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	verifier := parts[1]
	// Clear the cookie so a stolen or replayed callback URL can't
	// reuse the same verifier.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/api/auth/oauth",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := c.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", verifier),
	)
	if err != nil {
		http.Error(w, "token exchange: "+err.Error(), http.StatusBadGateway)
		return
	}
	email, subject, name, err := fetchUserinfo(ctx, c, tok, provider)
	if err != nil {
		http.Error(w, "userinfo: "+err.Error(), http.StatusBadGateway)
		return
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" || subject == "" {
		http.Error(w, "provider returned incomplete profile", http.StatusBadGateway)
		return
	}

	users := h.auth.UserService()
	// Prefer lookup by provider id: handles email renames at the
	// provider without creating a second row.
	u, err := users.GetByAuthProvider(ctx, provider, subject)
	if err != nil && !errors.Is(err, user.ErrNotFound) {
		http.Error(w, "lookup user: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if u == nil {
		// Fall back to email lookup. Users must already exist in some
		// workspace; SSO login does not implicitly create a new
		// workspace. Invited users join via the guest-invite flow.
		u, err = users.GetByEmailAnyWorkspace(ctx, email)
		if err != nil {
			if errors.Is(err, user.ErrNotFound) {
				http.Error(w, "no zk-drive account for "+email, http.StatusForbidden)
				return
			}
			http.Error(w, "lookup user: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := users.LinkAuthProvider(ctx, u.ID, provider, subject); err != nil {
			http.Error(w, "link provider: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if h.audit != nil {
			actor := u.ID
			h.audit.LogAction(ctx, u.WorkspaceID, &actor, audit.ActionSSOLink, "", nil, r, map[string]any{
				"provider": provider,
				"email":    email,
			})
		}
	}
	if u.DeactivatedAt != nil {
		http.Error(w, "account deactivated", http.StatusForbidden)
		return
	}
	if err := users.UpdateLastLogin(ctx, u.ID, time.Now().UTC()); err != nil && !errors.Is(err, user.ErrNotFound) {
		log.Printf("sso: update last login: %v", err)
	}
	if h.audit != nil {
		actor := u.ID
		h.audit.LogAction(ctx, u.WorkspaceID, &actor, audit.ActionSSOLogin, "", nil, r, map[string]any{
			"provider": provider,
			"email":    email,
			"name":     name,
		})
	}
	h.auth.WriteToken(w, u.ID, u.WorkspaceID, u.Role)
}

// fetchUserinfo hits the provider's userinfo endpoint and returns
// (email, subject, name). The same function handles Google and
// Microsoft by switching on provider.
func fetchUserinfo(ctx context.Context, c *oauth2.Config, tok *oauth2.Token, provider string) (string, string, string, error) {
	var endpoint string
	switch provider {
	case ProviderGoogle:
		endpoint = "https://openidconnect.googleapis.com/v1/userinfo"
	case ProviderMicrosoft:
		endpoint = "https://graph.microsoft.com/v1.0/me"
	default:
		return "", "", "", fmt.Errorf("unknown provider %q", provider)
	}
	client := c.Client(ctx, tok)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", "", fmt.Errorf("userinfo %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	switch provider {
	case ProviderGoogle:
		var p struct {
			Sub   string `json:"sub"`
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return "", "", "", err
		}
		return p.Email, p.Sub, p.Name, nil
	case ProviderMicrosoft:
		var p struct {
			ID                string `json:"id"`
			Mail              string `json:"mail"`
			UserPrincipalName string `json:"userPrincipalName"`
			DisplayName       string `json:"displayName"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return "", "", "", err
		}
		email := p.Mail
		if email == "" {
			email = p.UserPrincipalName
		}
		return email, p.ID, p.DisplayName, nil
	}
	return "", "", "", fmt.Errorf("unknown provider %q", provider)
}

// randomString returns a URL-safe random string of the given byte
// length (encoded base64url, no padding). Used for state and PKCE
// verifier.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge returns the S256 code challenge for a verifier per
// RFC 7636.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// uuidPtr is a tiny helper used by callers in other packages that
// want to pass a *uuid.UUID from a value.
func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }

var (
	_ = url.PathEscape // silence unused in trimmed builds
	_ = uuidPtr
)
