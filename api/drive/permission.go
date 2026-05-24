package drive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// Permission DTOs ----------------------------------------------------------

type grantPermissionRequest struct {
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	GranteeType  string  `json:"grantee_type"`
	GranteeID    string  `json:"grantee_id"`
	Role         string  `json:"role"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
}

// ListPermissions returns every grant on a resource. Callers must supply
// resource_type and resource_id query params. Scoped to the authenticated
// workspace so one tenant never sees another's grants.
func (h *Handler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	if h.permissions == nil {
		http.Error(w, "permissions not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())

	resourceType := r.URL.Query().Get("resource_type")
	resourceIDParam := r.URL.Query().Get("resource_id")
	if resourceType == "" || resourceIDParam == "" {
		http.Error(w, "resource_type and resource_id are required", http.StatusBadRequest)
		return
	}
	resourceID, err := uuid.Parse(resourceIDParam)
	if err != nil {
		http.Error(w, "invalid resource_id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, resourceType, resourceID); err != nil {
		writeServiceError(w, err)
		return
	}
	list, err := h.permissions.ListForResource(r.Context(), workspaceID, resourceType, resourceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissions": list})
}

// GrantPermission creates a new permission. Admin-only.
func (h *Handler) GrantPermission(w http.ResponseWriter, r *http.Request) {
	if h.permissions == nil {
		http.Error(w, "permissions not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())

	var req grantPermissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	resourceID, err := uuid.Parse(req.ResourceID)
	if err != nil {
		http.Error(w, "invalid resource_id", http.StatusBadRequest)
		return
	}
	granteeID, err := uuid.Parse(req.GranteeID)
	if err != nil {
		http.Error(w, "invalid grantee_id", http.StatusBadRequest)
		return
	}

	// Resources must live in the same workspace — otherwise an admin in
	// workspace A could grant access to a resource in workspace B by
	// guessing its UUID. This is the core tenant-isolation check for the
	// permissions API.
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, req.ResourceType, resourceID); err != nil {
		writeServiceError(w, err)
		return
	}
	if req.GranteeType == permission.GranteeUser {
		if err := h.assertUserInWorkspace(r.Context(), workspaceID, granteeID); err != nil {
			writeServiceError(w, err)
			return
		}
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			http.Error(w, "invalid expires_at (expected RFC3339)", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}

	p, err := h.permissions.Grant(r.Context(), workspaceID, req.ResourceType, resourceID, req.GranteeType, granteeID, req.Role, expiresAt)
	if err != nil {
		writePermissionError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionPermGrant, req.ResourceType, resourceID, map[string]any{
		"permission_id": p.ID,
		"grantee_type":  p.GranteeType,
		"grantee_id":    p.GranteeID,
		"role":          p.Role,
	})
	permID := p.ID
	h.logAudit(r.Context(), r, audit.ActionPermissionGrant, req.ResourceType, &permID, map[string]any{
		"resource_id":  resourceID,
		"grantee_type": p.GranteeType,
		"grantee_id":   p.GranteeID,
		"role":         p.Role,
	})
	h.publishWebhookPermissionEvent(r.Context(), webhooks.EventPermissionGranted, workspaceID, p.ResourceType, p.ResourceID, p.GranteeID, p.Role)
	writeJSON(w, http.StatusCreated, p)
}

// RevokePermission deletes a grant. Admin-only.
func (h *Handler) RevokePermission(w http.ResponseWriter, r *http.Request) {
	if h.permissions == nil {
		http.Error(w, "permissions not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	// Fetch first so we can log the resource context on revoke.
	p, err := h.permissions.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.permissions.Revoke(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionPermRevoke, p.ResourceType, p.ResourceID, map[string]any{
		"permission_id": p.ID,
		"grantee_type":  p.GranteeType,
		"grantee_id":    p.GranteeID,
		"role":          p.Role,
	})
	permID := p.ID
	h.logAudit(r.Context(), r, audit.ActionPermissionRevoke, p.ResourceType, &permID, map[string]any{
		"resource_id":  p.ResourceID,
		"grantee_type": p.GranteeType,
		"grantee_id":   p.GranteeID,
		"role":         p.Role,
	})
	h.publishWebhookPermissionEvent(r.Context(), webhooks.EventPermissionRevoked, workspaceID, p.ResourceType, p.ResourceID, p.GranteeID, p.Role)
	w.WriteHeader(http.StatusNoContent)
}

// assertResourceAccess gates a handler operation behind the
// permission layer using the inheritance-aware check (ARCHITECTURE.md
// §7.2). Workspace admins always pass; other authenticated users must
// hold a direct or inherited grant of at least minRole on the
// resource. When the permissions service is nil (metadata-only test
// wiring) the check is skipped so callers that opt out of permission
// enforcement keep working. Returns nil on allow, a forbiddenErr on
// deny, or the underlying repository error on lookup failure.
func (h *Handler) assertResourceAccess(ctx context.Context, resourceType string, resourceID uuid.UUID, minRole string) error {
	if h.permissions == nil {
		return nil
	}
	if role, _ := middleware.RoleFromContext(ctx); role == user.RoleAdmin {
		return nil
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(ctx)
	if !ok {
		return forbiddenErr{"missing workspace context"}
	}
	userID, ok := middleware.UserIDFromContext(ctx)
	if !ok {
		return forbiddenErr{"missing user context"}
	}
	allowed, err := h.permissions.HasAccessWithInheritance(ctx, workspaceID, resourceType, resourceID, permission.GranteeUser, userID, minRole)
	if err != nil {
		return err
	}
	if !allowed {
		return forbiddenErr{"insufficient permissions on resource"}
	}
	return nil
}

// assertResourceInWorkspace verifies that the given resource belongs to the
// workspace. Used by every permission endpoint to prevent cross-tenant
// grants.
func (h *Handler) assertResourceInWorkspace(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) error {
	switch resourceType {
	case permission.ResourceFolder:
		_, err := h.folders.GetByID(ctx, workspaceID, resourceID)
		return err
	case permission.ResourceFile:
		_, err := h.files.GetByID(ctx, workspaceID, resourceID)
		return err
	default:
		return badRequestErr{"invalid resource_type"}
	}
}

// assertUserInWorkspace verifies that granteeID corresponds to a user in
// this workspace. (Guest grantees are opaque UUIDs; they don't require a
// users-table lookup.)
func (h *Handler) assertUserInWorkspace(ctx context.Context, workspaceID, userID uuid.UUID) error {
	_, err := h.users.GetByID(ctx, workspaceID, userID)
	return err
}

func writePermissionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, permission.ErrInvalidRole),
		errors.Is(err, permission.ErrInvalidResourceType),
		errors.Is(err, permission.ErrInvalidGranteeType):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeServiceError(w, err)
	}
}
