package drive

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// Workspace DTOs -------------------------------------------------------------

type createWorkspaceRequest struct {
	Name string `json:"name"`
}

type updateWorkspaceRequest struct {
	Name              *string `json:"name,omitempty"`
	StorageQuotaBytes *int64  `json:"storage_quota_bytes,omitempty"`
	Tier              *string `json:"tier,omitempty"`
}

// ListWorkspaces returns all workspaces the authenticated user belongs to.
func (h *Handler) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	list, err := h.workspaces.ListForUser(r.Context(), userID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "list workspaces: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": list})
}

// CreateWorkspace creates a new workspace and makes the authenticated user
// its admin.
//
// Admin-only: creating a workspace makes the caller an admin of the
// new workspace, which is effectively a privilege grant — a member of
// an existing workspace must not be able to self-promote to admin of
// a fresh workspace they spin up alongside. Members can still BE
// invited to additional workspaces (and become admin there if invited
// as admin), but the act of creating a workspace from scratch
// requires they already hold admin in their current workspace.
//
// First-time signup is unaffected: the signup flow (api/auth.Signup)
// creates a workspace + first admin user atomically via a separate
// /api/auth/signup endpoint that does not pass through this handler.
func (h *Handler) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeAdminOnly, "admin role required")
		return
	}
	var req createWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.Name == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "name is required")
		return
	}

	// Look up the current user to copy identity + password hash into the new
	// workspace so the creator remains a member.
	currentWSID, _ := middleware.WorkspaceIDFromContext(r.Context())
	current, err := h.users.GetByID(r.Context(), currentWSID, userID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "load current user: "+err.Error())
		return
	}

	ws, err := h.createWorkspaceTx(r.Context(), req.Name, current)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "create workspace: "+err.Error())
		return
	}
	// Audit the privileged action so a security review can answer
	// "who spun up a new workspace and when". Best-effort: a nil
	// audit service silently drops via h.logAudit. Note that the
	// audit entry is scoped to the SOURCE workspace (the admin's
	// current workspace) so the chronology of admin actions for a
	// workspace stays together in one timeline; the new workspace's
	// id is recorded in the resource_id column (ws.ID), so the
	// metadata only needs to carry context that ISN'T already on
	// the audit row (i.e. the human-readable name).
	h.logAudit(r.Context(), r, audit.ActionWorkspaceCreate, "workspace", &ws.ID, map[string]any{
		"new_workspace_name": ws.Name,
	})
	writeJSON(w, http.StatusCreated, ws)
}

// createWorkspaceTx creates the workspace, adds the current user as its
// admin member, and sets workspace.owner_user_id — all inside a single
// transaction so partial failures don't leave orphaned rows.
func (h *Handler) createWorkspaceTx(ctx context.Context, name string, current *user.User) (*workspace.Workspace, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ws, err := h.workspaces.CreateTx(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	newUser := &user.User{
		WorkspaceID:  ws.ID,
		Email:        current.Email,
		Name:         current.Name,
		PasswordHash: current.PasswordHash,
		Role:         user.RoleAdmin,
	}
	if err := h.users.CreatePreservingHashTx(ctx, tx, newUser); err != nil {
		return nil, err
	}
	if err := h.workspaces.SetOwnerTx(ctx, tx, ws.ID, newUser.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	ws.OwnerUserID = &newUser.ID
	return ws, nil
}

// GetWorkspace returns workspace details. The authenticated session must be
// bound to this workspace.
func (h *Handler) GetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := h.requireWorkspaceMatch(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// UpdateWorkspace updates the workspace name, tier, or quota. Admin only.
func (h *Handler) UpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeAdminOnly, "admin role required")
		return
	}
	ws, err := h.requireWorkspaceMatch(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var req updateWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.Name != nil {
		ws.Name = *req.Name
	}
	if req.Tier != nil {
		ws.Tier = *req.Tier
	}
	if req.StorageQuotaBytes != nil {
		ws.StorageQuotaBytes = *req.StorageQuotaBytes
	}
	if err := h.workspaces.Update(r.Context(), ws); err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "update workspace: "+err.Error())
		return
	}
	wsID := ws.ID
	h.logAudit(r.Context(), r, audit.ActionWorkspaceUpdate, "workspace", &wsID, map[string]any{
		"name": ws.Name,
		"tier": ws.Tier,
	})
	writeJSON(w, http.StatusOK, ws)
}
