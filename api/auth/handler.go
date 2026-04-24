package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// Handler serves authentication HTTP endpoints.
type Handler struct {
	pool       *pgxpool.Pool
	users      *user.Service
	workspaces *workspace.Service
	audit      *audit.Service
	jwtSecret  string
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

// Signup creates a workspace, the first admin user, and returns a JWT.
func (h *Handler) Signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	req.WorkspaceName = strings.TrimSpace(req.WorkspaceName)
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	if req.WorkspaceName == "" || req.Email == "" || req.Name == "" || req.Password == "" {
		http.Error(w, "workspace_name, email, name, password are required", http.StatusBadRequest)
		return
	}

	ws, u, err := h.runSignupTx(r.Context(), req)
	if err != nil {
		http.Error(w, "signup: "+err.Error(), http.StatusInternalServerError)
		return
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
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
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
			http.Error(w, "invalid workspace_id", http.StatusBadRequest)
			return
		}
		u, err = h.users.GetByEmail(ctx, wsID, req.Email)
	} else {
		u, err = h.users.GetByEmailAnyWorkspace(ctx, req.Email)
	}
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		http.Error(w, "login: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.users.VerifyPassword(u, req.Password); err != nil {
		h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "password_mismatch",
		})
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if u.DeactivatedAt != nil {
		h.logAudit(ctx, u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "deactivated",
		})
		http.Error(w, "account deactivated", http.StatusForbidden)
		return
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

// Logout is a no-op for stateless JWTs — the client discards the token. The
// endpoint still responds 200 so clients can treat logout uniformly.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if claims, ok := middleware.ClaimsFromContext(r.Context()); ok {
		actor := claims.UserID
		h.logAudit(r.Context(), claims.WorkspaceID, &actor, audit.ActionLogout, r, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Refresh issues a new JWT with a fresh expiry for an already-authenticated
// user.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	writeToken(w, h.jwtSecret, claims.UserID, claims.WorkspaceID, claims.Role)
}

func writeToken(w http.ResponseWriter, secret string, userID, workspaceID uuid.UUID, role string) {
	token, exp, err := middleware.IssueToken(secret, userID, workspaceID, role, middleware.TokenTTL)
	if err != nil {
		http.Error(w, "issue token: "+err.Error(), http.StatusInternalServerError)
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
