package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"

	"github.com/kennguy3n/zk-drive/api/drive"
	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/iamcore"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// mockIDP is a stand-in for iam-core's OIDC surface, exposing exactly
// the four endpoints zk-drive depends on: OIDC discovery, the JWKS, the
// authorize redirect target, and the token endpoint. Tokens are signed
// with an in-process RSA key whose public half is published at the
// JWKS endpoint, so the real Verifier validates them end-to-end against
// the same code path production uses — no signature verification is
// stubbed out.
type mockIDP struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string

	mu    sync.Mutex
	codes map[string]string // authorization code -> PKCE S256 challenge
}

// newMockIDP starts an httptest server serving the OIDC discovery
// document, JWKS, authorize, and token endpoints for the given
// audience. The caller closes it via t.Cleanup.
func newMockIDP(t *testing.T, audience string) *mockIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := &mockIDP{
		key:      key,
		kid:      "test-key-1",
		audience: audience,
		codes:    make(map[string]string),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]any{
			"issuer":                 idp.issuer,
			"authorization_endpoint": idp.issuer + "/oauth2/authorize",
			"token_endpoint":         idp.issuer + "/oauth2/token",
			"jwks_uri":               idp.issuer + "/.well-known/jwks.json",
			"userinfo_endpoint":      idp.issuer + "/userinfo",
		})
	})
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		pub := idp.key.Public().(*rsa.PublicKey)
		// Exercise the Cache-Control max-age path in the verifier;
		// 300s is below minTTL so it clamps to 5m, which is fine.
		w.Header().Set("Cache-Control", "public, max-age=300")
		writeJSONResponse(w, map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": idp.kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			code := r.Form.Get("code")
			verifier := r.Form.Get("code_verifier")
			idp.mu.Lock()
			challenge, ok := idp.codes[code]
			delete(idp.codes, code)
			idp.mu.Unlock()
			if !ok {
				http.Error(w, "unknown code", http.StatusBadRequest)
				return
			}
			sum := sha256.Sum256([]byte(verifier))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				http.Error(w, "pkce mismatch", http.StatusBadRequest)
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") == "" {
				http.Error(w, "missing refresh_token", http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, "unsupported grant", http.StatusBadRequest)
			return
		}
		access := idp.mint(t, mockClaims{
			Subject:  "exchange-subject",
			Email:    "exchange@tenant.example",
			TenantID: "exchange-tenant",
			Roles:    []string{"member"},
			TTL:      time.Hour,
		})
		writeJSONResponse(w, map[string]any{
			"access_token":  access,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "refresh-token-value",
			"scope":         "openid email profile offline_access",
		})
	})

	srv := httptest.NewServer(mux)
	idp.server = srv
	idp.issuer = srv.URL
	t.Cleanup(srv.Close)
	return idp
}

// mockClaims is the set of claim values a test wants minted into an
// access token. Zero values are omitted so a test can, e.g., leave both
// tenant fields empty to exercise the fail-closed no-tenant path.
type mockClaims struct {
	Issuer   string // defaults to the IdP issuer when empty
	Audience string // defaults to the IdP audience when empty
	Subject  string
	Email    string
	Name     string
	OrgID    string
	TenantID string
	Roles    []string
	TTL      time.Duration // token lifetime relative to now; negative = already expired
}

// mint signs an RS256 access token for the given claims using the IdP's
// key and kid, matching the token shape iam-core emits.
func (idp *mockIDP) mint(t *testing.T, c mockClaims) string {
	t.Helper()
	now := time.Now()
	iss := c.Issuer
	if iss == "" {
		iss = idp.issuer
	}
	aud := c.Audience
	if aud == "" {
		aud = idp.audience
	}
	claims := jwt.MapClaims{
		"iss": iss,
		"sub": c.Subject,
		"iat": now.Add(-time.Minute).Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
		"exp": now.Add(c.TTL).Unix(),
	}
	if aud != "" {
		claims["aud"] = aud
	}
	if c.Email != "" {
		claims["email"] = c.Email
	}
	if c.Name != "" {
		claims["name"] = c.Name
	}
	if c.OrgID != "" {
		claims["org_id"] = c.OrgID
	}
	if c.TenantID != "" {
		claims["tenant_id"] = c.TenantID
	}
	if c.Roles != nil {
		claims["roles"] = c.Roles
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = idp.kid
	signed, err := tok.SignedString(idp.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// issueCode registers an authorization code bound to a PKCE challenge,
// emulating what iam-core stores when the browser completes the
// authorize step. The returned code is then redeemable exactly once at
// the token endpoint.
func (idp *mockIDP) issueCode(challenge string) string {
	code := "auth-code-" + challenge[:8]
	idp.mu.Lock()
	idp.codes[code] = challenge
	idp.mu.Unlock()
	return code
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// iamCoreEnv bundles a minimal data-plane wired with the real iam-core
// middleware in front of two genuine drive endpoints (/api/me and
// /api/folders). It reuses the shared testEnv HTTP helpers.
type iamCoreEnv struct {
	*testEnv
	idp    *mockIDP
	client *iamcore.Client
}

// setupIAMCoreEnv connects to Postgres, runs migrations, wires the
// iam-core auth front-end exactly as cmd/server/main.go does (Verifier
// + TenantMapper + Middleware), and mounts /api/me and /api/folders
// behind it so a test can prove a verified token resolves a workspace
// and the drive API works end-to-end.
func setupIAMCoreEnv(t *testing.T) *iamCoreEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	const audience = "zk-drive"
	idp := newMockIDP(t, audience)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	if err := database.Migrate(ctx, pool, findMigrationsDir(t)); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}

	userSvc := user.NewService(user.NewPostgresRepository(pool))
	wsSvc := workspace.NewService(workspace.NewPostgresRepository(pool))
	folderSvc := folder.NewService(folder.NewPostgresRepository(pool))
	fileSvc := file.NewService(file.NewPostgresRepository(pool))
	permissionSvc := permission.NewService(permission.NewPostgresRepository(pool))
	activitySvc := activity.NewService(activity.NewPostgresRepository(pool))
	auditSvc := audit.NewService(audit.NewPostgresRepository(pool))
	storageClient := buildTestStorageClient(t)

	client, err := iamcore.NewClient(ctx, iamcore.Config{
		IssuerURL:   idp.issuer,
		ClientID:    "zk-drive-spa",
		Audience:    audience,
		CallbackURL: "http://localhost:5173/auth/callback",
	}, idp.server.Client())
	if err != nil {
		pool.Close()
		t.Fatalf("iamcore client: %v", err)
	}
	iamCoreMW := iamcore.NewMiddleware(client.NewVerifier(), iamcore.NewTenantMapper(pool, wsSvc), userSvc).
		WithAudit(auditSvc)

	driveHandler := drive.NewHandler(pool, wsSvc, folderSvc, fileSvc, userSvc, storageClient, permissionSvc, activitySvc).
		WithAudit(auditSvc)

	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(iamCoreMW.Handler)
			r.Use(middleware.TenantGuard())
			r.Get("/me", driveHandler.Me)
			r.Get("/folders", driveHandler.ListFolders)
			r.Post("/folders", driveHandler.CreateFolder)
		})
	})

	srv := httptest.NewServer(r)
	env := &testEnv{t: t, pool: pool, server: srv}
	t.Cleanup(func() {
		srv.Close()
		activitySvc.Close()
		auditSvc.Close()
		pool.Close()
	})
	env.ResetTables()
	return &iamCoreEnv{testEnv: env, idp: idp, client: client}
}

type meResponse struct {
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	Role        string `json:"role"`
	Email       string `json:"email"`
	Name        string `json:"name"`
}

// TestIAMCoreValidTokenResolvesWorkspaceAndDriveAPIWorks is the happy
// path: a valid iam-core access token authenticates, the tenant is
// mapped to a freshly provisioned workspace, a federated user row is
// created, and a genuine drive endpoint (folder create + list) works
// under the resolved workspace's row-level-security scope.
func TestIAMCoreValidTokenResolvesWorkspaceAndDriveAPIWorks(t *testing.T) {
	env := setupIAMCoreEnv(t)

	token := env.idp.mint(t, mockClaims{
		Subject:  "user-sub-1",
		Email:    "alice@acme.example",
		Name:     "Alice Admin",
		OrgID:    "org-acme",
		TenantID: "tenant-acme",
		Roles:    []string{"admin"},
		TTL:      time.Hour,
	})

	status, body := env.httpRequest(http.MethodGet, "/api/me", token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/me: status=%d body=%s", status, body)
	}
	var me meResponse
	env.decodeJSON(body, &me)
	if me.WorkspaceID == "" || me.UserID == "" {
		t.Fatalf("expected resolved identity, got %+v", me)
	}
	if me.Role != user.RoleAdmin {
		t.Fatalf("expected role %q, got %q", user.RoleAdmin, me.Role)
	}
	if me.Email != "alice@acme.example" {
		t.Fatalf("expected enriched email, got %q", me.Email)
	}

	// The (tenant, org) pair must now map to the resolved workspace.
	ctx := context.Background()
	var mappedWS string
	if err := env.pool.QueryRow(ctx,
		`SELECT workspace_id::text FROM iam_core_tenant_workspaces WHERE iam_tenant_id=$1 AND iam_org_id=$2`,
		"tenant-acme", "org-acme",
	).Scan(&mappedWS); err != nil {
		t.Fatalf("query tenant mapping: %v", err)
	}
	if mappedWS != me.WorkspaceID {
		t.Fatalf("mapping workspace %s != resolved %s", mappedWS, me.WorkspaceID)
	}

	// A passwordless federated user row must exist for the subject.
	var provider, providerID string
	if err := env.pool.QueryRow(ctx,
		`SELECT auth_provider, auth_provider_id FROM users WHERE id=$1`, me.UserID,
	).Scan(&provider, &providerID); err != nil {
		t.Fatalf("query user: %v", err)
	}
	if provider != iamcore.Provider || providerID != "user-sub-1" {
		t.Fatalf("expected federated user (%s,%s), got (%s,%s)", iamcore.Provider, "user-sub-1", provider, providerID)
	}

	// Drive API works under the resolved workspace: create then list.
	status, body = env.httpRequest(http.MethodPost, "/api/folders", token, map[string]any{"name": "Engineering"})
	if status != http.StatusCreated {
		t.Fatalf("POST /api/folders: status=%d body=%s", status, body)
	}
	status, body = env.httpRequest(http.MethodGet, "/api/folders", token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/folders: status=%d body=%s", status, body)
	}
}

// TestIAMCoreSecondLoginReusesWorkspace verifies a returning user from
// the same tenant resolves to the same workspace and user row (no
// duplicate provisioning), and that a second subject in the same tenant
// joins the same workspace.
func TestIAMCoreSecondLoginReusesWorkspace(t *testing.T) {
	env := setupIAMCoreEnv(t)

	tok1 := env.idp.mint(t, mockClaims{Subject: "sub-a", Email: "a@t.example", TenantID: "tenant-x", Roles: []string{"member"}, TTL: time.Hour})
	tok2 := env.idp.mint(t, mockClaims{Subject: "sub-b", Email: "b@t.example", TenantID: "tenant-x", Roles: []string{"member"}, TTL: time.Hour})

	var me1, me2 meResponse
	status, body := env.httpRequest(http.MethodGet, "/api/me", tok1, nil)
	if status != http.StatusOK {
		t.Fatalf("first login: status=%d body=%s", status, body)
	}
	env.decodeJSON(body, &me1)
	status, body = env.httpRequest(http.MethodGet, "/api/me", tok2, nil)
	if status != http.StatusOK {
		t.Fatalf("second user login: status=%d body=%s", status, body)
	}
	env.decodeJSON(body, &me2)

	if me1.WorkspaceID != me2.WorkspaceID {
		t.Fatalf("same tenant must share workspace: %s vs %s", me1.WorkspaceID, me2.WorkspaceID)
	}
	if me1.UserID == me2.UserID {
		t.Fatalf("distinct subjects must be distinct users")
	}

	// Exactly one workspace and one tenant mapping for tenant-x.
	var wsCount, mapCount int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM iam_core_tenant_workspaces WHERE iam_tenant_id=$1`, "tenant-x",
	).Scan(&mapCount); err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if mapCount != 1 {
		t.Fatalf("expected 1 tenant mapping, got %d", mapCount)
	}
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workspaces`,
	).Scan(&wsCount); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if wsCount != 1 {
		t.Fatalf("expected exactly 1 provisioned workspace, got %d", wsCount)
	}
}

// TestIAMCoreExpiredTokenRejected confirms an expired access token is
// rejected with 401 and provisions nothing.
func TestIAMCoreExpiredTokenRejected(t *testing.T) {
	env := setupIAMCoreEnv(t)
	token := env.idp.mint(t, mockClaims{
		Subject:  "expired-sub",
		TenantID: "tenant-exp",
		Roles:    []string{"member"},
		TTL:      -time.Hour,
	})
	status, body := env.httpRequest(http.MethodGet, "/api/me", token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d body=%s", status, body)
	}
}

// TestIAMCoreWrongIssuerRejected confirms a token whose `iss` claim
// does not match the configured issuer is rejected, even though it is
// signed by the same (trusted) key — defending against token reuse
// across relying parties.
func TestIAMCoreWrongIssuerRejected(t *testing.T) {
	env := setupIAMCoreEnv(t)
	token := env.idp.mint(t, mockClaims{
		Issuer:   "https://evil.example.com",
		Subject:  "wrong-iss-sub",
		TenantID: "tenant-evil",
		Roles:    []string{"admin"},
		TTL:      time.Hour,
	})
	status, body := env.httpRequest(http.MethodGet, "/api/me", token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong issuer, got %d body=%s", status, body)
	}
}

// TestIAMCoreWrongAudienceRejected confirms a token minted for a
// different audience cannot be replayed against zk-drive.
func TestIAMCoreWrongAudienceRejected(t *testing.T) {
	env := setupIAMCoreEnv(t)
	token := env.idp.mint(t, mockClaims{
		Audience: "some-other-rp",
		Subject:  "wrong-aud-sub",
		TenantID: "tenant-aud",
		Roles:    []string{"member"},
		TTL:      time.Hour,
	})
	status, body := env.httpRequest(http.MethodGet, "/api/me", token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong audience, got %d body=%s", status, body)
	}
}

// TestIAMCoreMissingTenantRejected confirms the fail-closed posture: a
// token with neither tenant_id nor org_id has no workspace to resolve
// and is rejected rather than landing in a default workspace.
func TestIAMCoreMissingTenantRejected(t *testing.T) {
	env := setupIAMCoreEnv(t)
	token := env.idp.mint(t, mockClaims{
		Subject: "no-tenant-sub",
		Email:   "lonely@nowhere.example",
		Roles:   []string{"member"},
		TTL:     time.Hour,
	})
	status, body := env.httpRequest(http.MethodGet, "/api/me", token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing tenant, got %d body=%s", status, body)
	}
}

// TestIAMCoreMissingBearerRejected confirms requests without an
// Authorization header are rejected.
func TestIAMCoreMissingBearerRejected(t *testing.T) {
	env := setupIAMCoreEnv(t)
	status, body := env.httpRequest(http.MethodGet, "/api/me", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing bearer, got %d body=%s", status, body)
	}
}

// TestIAMCorePKCEExchange exercises the OAuth2 Authorization Code +
// PKCE token exchange against the mock IdP: a verifier/challenge pair
// is generated, an authorization code is issued for the challenge, and
// the client redeems it with the verifier to obtain a usable access
// token that then authenticates a drive request.
func TestIAMCorePKCEExchange(t *testing.T) {
	env := setupIAMCoreEnv(t)

	verifier, challenge, err := iamcore.NewPKCE()
	if err != nil {
		t.Fatalf("new pkce: %v", err)
	}
	code := env.idp.issueCode(challenge)

	tr, err := env.client.Exchange(context.Background(), code, verifier)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tr.AccessToken == "" {
		t.Fatalf("expected access token from exchange")
	}

	status, body := env.httpRequest(http.MethodGet, "/api/me", tr.AccessToken, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/me with exchanged token: status=%d body=%s", status, body)
	}

	// Exchanging with the wrong verifier must fail (PKCE binding).
	code2 := env.idp.issueCode(challenge)
	if _, err := env.client.Exchange(context.Background(), code2, "wrong-verifier"); err == nil {
		t.Fatalf("expected PKCE mismatch to fail exchange")
	}
}

// TestIAMCoreDisabledFallsBackToBuiltinAuth confirms the backward-
// compatible fallback: with IAM_CORE_ISSUER_URL empty, iam-core is
// disabled (the server wires built-in auth instead), and the built-in
// signup/login path continues to authenticate and serve the drive API.
func TestIAMCoreDisabledFallsBackToBuiltinAuth(t *testing.T) {
	// Config-level gate: an empty issuer disables iam-core, and
	// constructing a client in that state is refused.
	if (iamcore.Config{}).Enabled() {
		t.Fatalf("empty IAM_CORE_ISSUER_URL must disable iam-core")
	}
	if _, err := iamcore.NewClient(context.Background(), iamcore.Config{}, nil); err == nil {
		t.Fatalf("NewClient must refuse a disabled config")
	}

	// Functional proof: the built-in auth harness still works, i.e.
	// when iam-core is not configured the existing password flow and
	// drive API remain fully operational.
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")
	status, body := env.httpRequest(http.MethodGet, "/api/me", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("built-in /api/me: status=%d body=%s", status, body)
	}
	var me meResponse
	env.decodeJSON(body, &me)
	if me.WorkspaceID != tok.WorkspaceID {
		t.Fatalf("built-in identity workspace mismatch: %s vs %s", me.WorkspaceID, tok.WorkspaceID)
	}
}
