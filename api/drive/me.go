package drive

import (
	"net/http"

	"github.com/kennguy3n/zk-drive/api/middleware"
)

// meResponse is the resolved identity of the authenticated caller. It
// is auth-mode agnostic: the fields are taken from the request context
// that whichever authenticator ran (built-in session JWT or the
// iam-core OIDC middleware) populated, so a single client contract
// works in both modes.
type meResponse struct {
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	Role        string `json:"role"`
	Email       string `json:"email,omitempty"`
	Name        string `json:"name,omitempty"`
}

// Me returns the authenticated caller's resolved zk-drive identity
// (user id, active workspace, role, and profile fields).
//
// It exists primarily for the iam-core OIDC flow: after the SPA
// exchanges an authorization code for an access token it has only the
// iam-core token, whose claims carry the upstream subject — NOT the
// zk-drive-internal user/workspace UUIDs the UI needs for admin gating
// and collaboration presence. Calling GET /api/me once after login (or
// after a silent token refresh) resolves those server-side, where the
// tenant mapping and user provisioning already happened. It is equally
// valid in built-in mode, where it simply echoes the session claims,
// so the SPA can use one code path for both.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := middleware.UserIDFromContext(ctx)
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(ctx)
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeNoWorkspace, "missing workspace context")
		return
	}
	role, _ := middleware.RoleFromContext(ctx)

	resp := meResponse{
		UserID:      userID.String(),
		WorkspaceID: workspaceID.String(),
		Role:        role,
	}
	// Best-effort profile enrichment. The identity above is
	// authoritative on its own; a lookup miss (e.g. a just-revoked
	// row) still returns the core identity rather than failing the
	// request.
	if u, err := h.users.GetByID(ctx, workspaceID, userID); err == nil {
		resp.Email = u.Email
		resp.Name = u.Name
	}
	writeJSON(w, http.StatusOK, resp)
}
