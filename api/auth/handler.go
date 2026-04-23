package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// Handler serves authentication HTTP endpoints.
type Handler struct {
	users      *user.Service
	workspaces *workspace.Service
	jwtSecret  string
}

// NewHandler constructs a Handler from the user and workspace services.
func NewHandler(users *user.Service, workspaces *workspace.Service, jwtSecret string) *Handler {
	return &Handler{users: users, workspaces: workspaces, jwtSecret: jwtSecret}
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

	ctx := r.Context()
	ws, err := h.workspaces.Create(ctx, req.WorkspaceName)
	if err != nil {
		http.Error(w, "create workspace: "+err.Error(), http.StatusInternalServerError)
		return
	}
	u, err := h.users.Create(ctx, ws.ID, req.Email, req.Name, req.Password, user.RoleAdmin)
	if err != nil {
		http.Error(w, "create user: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.workspaces.SetOwner(ctx, ws.ID, u.ID); err != nil {
		http.Error(w, "set workspace owner: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeToken(w, h.jwtSecret, u.ID, ws.ID, u.Role)
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
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	writeToken(w, h.jwtSecret, u.ID, u.WorkspaceID, u.Role)
}

// Logout is a no-op for stateless JWTs — the client discards the token. The
// endpoint still responds 200 so clients can treat logout uniformly.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
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
