package auth

import (
	"context"
	"encoding/json"
	"errors"

	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/totp"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// PostSignupHook is invoked after the signup transaction commits. It
// is best-effort: a non-nil error is logged via Hook's own logging
// path but never propagated to the HTTP response, so workspace
// creation succeeds even when downstream side-effects (e.g. fabric
// tenant provisioning) fail. Callers wire this with WithPostSignupHook.
type PostSignupHook func(ctx context.Context, workspaceID uuid.UUID, workspaceName string)

// SessionRevoker is the small subset of session.RedisSessionStore the
// auth handler needs: it extends middleware.SessionChecker (which
// already declares IsRevoked) with the write-side RevokeUser hook so
// Logout can record per-user cutoffs. Embedding the middleware
// interface — rather than redeclaring IsRevoked here — means a
// signature change in one place propagates everywhere via the type
// system, eliminating the silent-drift hazard a duplicated method
// declaration would carry.
//
// Pulled behind its own interface (rather than depending directly on
// session.RedisSessionStore) so tests can substitute an in-memory
// implementation without spinning up Redis and so api/auth keeps a
// no-cycle dependency on internal/session.
type SessionRevoker interface {
	middleware.SessionChecker
	RevokeUser(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time, ttl time.Duration) error
}

// Handler serves authentication HTTP endpoints.
type Handler struct {
	pool       *pgxpool.Pool
	users      *user.Service
	workspaces *workspace.Service
	audit      *audit.Service
	jwtSecret  string
	postSignup PostSignupHook
	sessions   SessionRevoker
	totp       *totp.Service
}

// NewHandler constructs a Handler from the user and workspace services. The
// pool is used to run multi-step writes (signup) atomically.
func NewHandler(pool *pgxpool.Pool, users *user.Service, workspaces *workspace.Service, jwtSecret string) *Handler {
	return &Handler{pool: pool, users: users, workspaces: workspaces, jwtSecret: jwtSecret}
}

// WithAudit attaches an audit service so login / logout / SSO events
// are recorded. Optional: handlers work when nil (fire-and-forget).
func (h *Handler) WithAudit(svc *audit.Service) *Handler {
	h.audit = svc
	return h
}

// WithPostSignupHook wires a callback invoked after a successful
// signup commit. The hook runs in the request goroutine but its
// errors do not affect the HTTP response — see PostSignupHook docs.
func (h *Handler) WithPostSignupHook(hook PostSignupHook) *Handler {
	h.postSignup = hook
	return h
}

// WithSessionRevoker wires the session store so Logout actually
// invalidates tokens. When nil (the test harness path, single-process
// dev mode), Logout is a no-op beyond the audit log entry — matching
// stateless-JWT behaviour. Production wiring in
// cmd/server installs the Redis-backed store unconditionally when
// REDIS_URL is configured.
func (h *Handler) WithSessionRevoker(s SessionRevoker) *Handler {
	h.sessions = s
	return h
}

// WithTOTP wires the TOTP service so Login can fork into the MFA
// challenge path when the user has 2FA enrolled, and so the
// /auth/totp/* endpoints can drive enrollment / verify / disable.
// Optional: when nil the auth handler behaves exactly as it did
// before the TOTP service was introduced (password-only logins).
func (h *Handler) WithTOTP(svc *totp.Service) *Handler {
	h.totp = svc
	return h
}

// TOTPService exposes the wired *totp.Service so the
// api/auth/totp.go endpoint handlers can share the same instance
// without each route taking its own service pointer.
func (h *Handler) TOTPService() *totp.Service { return h.totp }

// logAudit is nil-safe so the integration test harness (which does not
// wire an audit service) keeps passing without code duplication.
func (h *Handler) logAudit(ctx context.Context, workspaceID uuid.UUID, actorID *uuid.UUID, action string, r *http.Request, metadata map[string]any) {
	if h.audit == nil {
		return
	}
	h.audit.LogAction(ctx, workspaceID, actorID, action, "", nil, r, metadata)
}

// UserService returns the underlying user service so downstream
// handlers (e.g. OAuth) can reuse the already-wired dependencies.
func (h *Handler) UserService() *user.Service { return h.users }

// WorkspaceService returns the underlying workspace service.
func (h *Handler) WorkspaceService() *workspace.Service { return h.workspaces }

// Pool returns the pgx pool used for multi-step writes.
func (h *Handler) Pool() *pgxpool.Pool { return h.pool }

// JWTSecret returns the HMAC secret used to sign issued tokens.
func (h *Handler) JWTSecret() string { return h.jwtSecret }

// WriteToken signs a new token and writes it as the HTTP response
// body. Exposed so other handlers (e.g. OAuth callbacks) can complete
// the same login flow without duplicating JWT issuance.
func (h *Handler) WriteToken(w http.ResponseWriter, userID, workspaceID uuid.UUID, role string) {
	writeToken(w, h.jwtSecret, userID, workspaceID, role)
}

type signupRequest struct {
	WorkspaceName string `json:"workspace_name"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	Password      string `json:"password"`
}

type loginRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	WorkspaceID string `json:"workspace_id"`
}

type tokenResponse struct {
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expires_at"`
	UserID      uuid.UUID `json:"user_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Role        string    `json:"role"`
}

// mfaChallengeResponse is returned by Login when the user has TOTP
// enrolled OR when the workspace requires MFA but the user has not
// yet enrolled. The client never receives a session JWT in this
// path — it must complete /auth/totp/verify (or /auth/totp/enroll
// for must_enroll users) to receive a real session token.
//
// The shape is intentionally NOT a tokenResponse subtype: a client
// that fails to inspect `mfa_required` and treats the body as a
// session token must fail loudly, not silently end up holding a
// challenge token in its session cookie.
type mfaChallengeResponse struct {
	MFARequired bool      `json:"mfa_required"`
	MFAToken    string    `json:"mfa_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	// MustEnroll signals that the workspace requires MFA but the
	// user has no credential yet. Clients should redirect to the
	// enrollment flow and use the supplied mfa_token (with
	// purpose=mfa_enroll) to authorize the enroll endpoints.
	MustEnroll bool `json:"must_enroll,omitempty"`
}

// Signup creates a workspace, the first admin user, and returns a JWT.
func (h *Handler) Signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	req.WorkspaceName = strings.TrimSpace(req.WorkspaceName)
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	if req.WorkspaceName == "" || req.Email == "" || req.Name == "" || req.Password == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "workspace_name, email, name, password are required")
		return
	}

	ws, u, err := h.runSignupTx(r.Context(), req)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "signup: "+err.Error())
		return
	}

	// Best-effort post-signup hook (fabric tenant provisioning). Any
	// error inside the hook is the hook's responsibility to log; we
	// must not let it block the HTTP response because the user-facing
	// signup is already durable on disk.
	if h.postSignup != nil {
		h.postSignup(r.Context(), ws.ID, ws.Name)
	}

	writeToken(w, h.jwtSecret, u.ID, ws.ID, u.Role)
}

// runSignupTx performs the workspace+user+owner writes in a single
// transaction so a partial failure never leaves an orphaned workspace or
// owner-less row behind.
func (h *Handler) runSignupTx(ctx context.Context, req signupRequest) (*workspace.Workspace, *user.User, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ws, u, err := signupInTx(ctx, tx, h.workspaces, h.users, req)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return ws, u, nil
}

func signupInTx(ctx context.Context, tx pgx.Tx, workspaces *workspace.Service, users *user.Service, req signupRequest) (*workspace.Workspace, *user.User, error) {
	ws, err := workspaces.CreateTx(ctx, tx, req.WorkspaceName)
	if err != nil {
		return nil, nil, err
	}
	u, err := users.CreateTx(ctx, tx, ws.ID, req.Email, req.Name, req.Password, user.RoleAdmin)
	if err != nil {
		return nil, nil, err
	}
	if err := workspaces.SetOwnerTx(ctx, tx, ws.ID, u.ID); err != nil {
		return nil, nil, err
	}
	return ws, u, nil
}

// Login validates credentials and returns a JWT.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "email and password are required")
		return
	}

	ctx := r.Context()

	var (
		u   *user.User
		err error
	)
	if req.WorkspaceID != "" {
		wsID, parseErr := uuid.Parse(req.WorkspaceID)
		if parseErr != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid workspace_id")
			return
		}
		u, err = h.users.GetByEmail(ctx, wsID, req.Email)
	} else {
		u, err = h.users.GetByEmailAnyWorkspace(ctx, req.Email)
	}
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			// Login flow: no user with that email. Distinct from
			// AUTH_INVALID_TOKEN — there is no session/token in play;
			// the user's credentials simply don't match. The locale
			// renders this as "Email or password is incorrect",
			// which is the actionable copy on the LoginPage form.
			middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthInvalidCredentials, "invalid credentials")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "login: "+err.Error())
		return
	}
	if err := h.users.VerifyPassword(u, req.Password); err != nil {
		h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "password_mismatch",
		})
		// Same code as user-not-found above: deliberately conflate
		// the two so login response timing doesn't expose whether
		// an email is registered.
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthInvalidCredentials, "invalid credentials")
		return
	}
	if u.DeactivatedAt != nil {
		h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "deactivated",
		})
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, "account deactivated")
		return
	}
	// Rehash-on-login: if the stored hash was created at a lower
	// bcrypt cost than the current PasswordHashCost (e.g. legacy
	// users created when the constant was 10, now bumped to 12),
	// upgrade the hash transparently. This is best-effort — a
	// failure here is logged but the login still succeeds, because
	// the cost bump is a defence-in-depth posture rather than a
	// hard auth requirement.
	//
	// Placed AFTER the DeactivatedAt check so a deactivated user
	// who supplies the correct password doesn't pay the bcrypt-12
	// CPU cost AND a UPDATE users SET password_hash + updated_at
	// write only to be immediately rejected with 403. Bumping
	// updated_at for an account that's about to be denied would
	// also mislead audit queries that use updated_at as an
	// activity signal.
	if err := h.users.MaybeRehashPassword(ctx, u, req.Password); err != nil {
		logging.FromContext(ctx).Error("auth rehash-on-login best-effort failed", "user_id", u.ID, "err", err)
	}

	// MFA fork. Run BEFORE UpdateLastLogin so the password-only
	// step never stamps last_login_at for a session that didn't
	// actually complete — last_login_at should reflect the
	// effective sign-in, which for MFA users only happens after
	// /auth/totp/verify. The verify handler is responsible for the
	// stamp on its happy path.
	if h.totp != nil {
		mfaResp, mfaErr := h.maybeIssueMFAChallenge(ctx, u, r)
		if mfaErr != nil {
			middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "mfa: "+mfaErr.Error())
			return
		}
		if mfaResp != nil {
			writeJSON(w, http.StatusOK, mfaResp)
			return
		}
	}

	if err := h.users.UpdateLastLogin(ctx, u.ID, time.Now().UTC()); err != nil && !errors.Is(err, user.ErrNotFound) {
		// Non-fatal: login still succeeds, but log the failure so we
		// can investigate a misconfigured pool.
		h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "success",
			"warn":   "update_last_login_failed",
		})
	} else {
		h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "success",
		})
	}

	writeToken(w, h.jwtSecret, u.ID, u.WorkspaceID, u.Role)
}

// maybeIssueMFAChallenge runs after a successful password verify
// to decide whether the user has to satisfy a second factor before
// receiving a session JWT.
//
// Returns (nil, nil) when the user can proceed with the
// password-only login: they have no TOTP credential AND the
// workspace does not require MFA.
//
// Returns (response, nil) with mfa_required=true in two cases:
//   - User has an ACTIVATED TOTP credential. Body carries a
//     purpose=mfa_challenge token; the client must POST it to
//     /auth/totp/verify with the live 6-digit code.
//   - Workspace requires MFA (mfa_required=true) but the user has
//     no activated credential. Body carries a purpose=mfa_enroll
//     token; the client must redirect to the enrollment flow.
//
// Audit-logs the auth.login event with result="mfa_required" so
// security operators can distinguish completed sign-ins from
// password-only ones that stalled at the second factor.
func (h *Handler) maybeIssueMFAChallenge(ctx context.Context, u *user.User, r *http.Request) (*mfaChallengeResponse, error) {
	status, err := h.totp.Status(ctx, u.ID)
	if err != nil {
		return nil, err
	}

	if status.Enabled {
		token, exp, err := middleware.IssueMFAChallengeToken(h.jwtSecret, u.ID, u.WorkspaceID)
		if err != nil {
			return nil, err
		}
		h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "mfa_required",
		})
		return &mfaChallengeResponse{
			MFARequired: true,
			MFAToken:    token,
			ExpiresAt:   exp,
		}, nil
	}

	// User has no active credential. Consult the workspace policy.
	ws, err := h.workspaces.GetByID(ctx, u.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !ws.MFARequired {
		return nil, nil
	}
	token, exp, err := middleware.IssueMFAEnrollToken(h.jwtSecret, u.ID, u.WorkspaceID)
	if err != nil {
		return nil, err
	}
	h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
		"result": "mfa_enrollment_required",
	})
	return &mfaChallengeResponse{
		MFARequired: true,
		MFAToken:    token,
		ExpiresAt:   exp,
		MustEnroll:  true,
	}, nil
}

// Logout records a per-user revocation cutoff in the session store
// so every token currently issued for the caller — including the
// one they just used to authenticate this request — is rejected by
// AuthMiddleware on subsequent requests. The endpoint still responds
// 204 even when the session store is unwired or fails, so clients
// can treat logout uniformly: the worst case (store unreachable) is
// that the JWT remains valid until its natural TTL elapses, which
// is the stateless-JWT fallback.
//
// We pass middleware.TokenTTL as the cutoff key's TTL so the
// underlying redis entry self-cleans after no token it could revoke
// remains valid. Without this the user_revoked: keys would
// accumulate forever for every logout in the system.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		// Defence-in-depth: production routes /logout behind
		// AuthMiddleware so this branch is normally unreachable —
		// the middleware would have already returned 401. We keep
		// the fallback so a future route that mounts Logout outside
		// the middleware group doesn't NPE on a nil claims pointer.
		// 204 (not 401) is the safer response in that case: the
		// client treats logout as fire-and-forget, and we'd rather
		// confirm than crash.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	actor := claims.UserID
	if h.sessions != nil {
		// Bound the RevokeUser call the same way AuthMiddleware
		// and Refresh bound their reads: a partial Redis stall
		// would otherwise hang /auth/logout for the server's
		// full WriteTimeout (30s) — and a hung logout endpoint
		// is the worst possible failure mode for a user trying
		// to terminate a possibly-compromised session. Fast
		// failure plus the audit-trail recovery path is the
		// right tradeoff.
		revokeCtx, cancel := context.WithTimeout(r.Context(), middleware.SessionCheckTimeout)
		err := h.sessions.RevokeUser(revokeCtx, claims.WorkspaceID, claims.UserID, time.Now().UTC(), middleware.TokenTTL)
		cancel()
		if err != nil {
			// Best-effort: log via audit but don't 500. The client
			// already considers itself logged out; surfacing 500
			// here would leave it confused. The audit trail is the
			// recovery mechanism if a store outage masked a real
			// revocation we needed to enforce.
			h.logAudit(r.Context(), claims.WorkspaceID, &actor, audit.ActionLogout, r, map[string]any{
				"result": "store_error",
				"error":  err.Error(),
			})
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	h.logAudit(r.Context(), claims.WorkspaceID, &actor, audit.ActionLogout, r, nil)
	w.WriteHeader(http.StatusNoContent)
}

// Refresh issues a new JWT with a fresh expiry for an already-authenticated
// user.
//
// We consult the session store before minting the new token to close
// the narrow race where: (a) the user calls Logout at time T, (b)
// Logout's RevokeUser hits Redis a few ms later, (c) a request that
// was already in flight with the about-to-be-revoked JWT lands on
// /auth/refresh before the cutoff is durable. AuthMiddleware would
// have let the request through (its check ran against the prior
// cutoff), and a naive Refresh handler would happily mint a new JWT
// from claims belonging to a now-revoked session. The extra check
// here means a Refresh inside the race window is rejected even
// though the gating middleware already let the request reach the
// handler.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	if h.sessions != nil {
		// Mirror AuthMiddleware's missing-iat handling: when a
		// revoker is wired, a token without an iat claim cannot
		// be checked against the cutoff and must be rejected
		// rather than silently bypassing the gate. The current
		// production wiring already enforces this in the
		// middleware before the handler runs, but a future
		// route that mounts Refresh outside the middleware
		// group (or a code path that constructs the handler
		// directly in tests) would otherwise mint a fresh
		// long-lived JWT without a revocation check.
		if claims.IssuedAt == nil {
			middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingIat, "token missing iat")
			return
		}
		// Bound the IsRevoked call the same way AuthMiddleware
		// does. Without this matching timeout, a partial Redis
		// outage that recovers just long enough for the
		// middleware's 1-second-bounded check to succeed could
		// still hang the Refresh handler for the full server
		// WriteTimeout — re-introducing the exact failure mode
		// the middleware timeout was designed to prevent.
		checkCtx, cancel := context.WithTimeout(r.Context(), middleware.SessionCheckTimeout)
		revoked, err := h.sessions.IsRevoked(checkCtx, claims.WorkspaceID, claims.UserID, claims.IssuedAt.Time)
		cancel()
		if err != nil {
			// Fail closed: a Refresh that cannot verify revocation
			// status must not mint a longer-lived token. The client
			// can retry once the store recovers.
			middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeRevocationCheck, "revocation check failed")
			return
		}
		if revoked {
			middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthRevokedToken, "token revoked")
			return
		}
	}
	writeToken(w, h.jwtSecret, claims.UserID, claims.WorkspaceID, claims.Role)
}

func writeToken(w http.ResponseWriter, secret string, userID, workspaceID uuid.UUID, role string) {
	token, exp, err := middleware.IssueToken(secret, userID, workspaceID, role, middleware.TokenTTL)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "issue token: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		Token:       token,
		ExpiresAt:   exp,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Role:        role,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
