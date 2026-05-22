package drive

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// Sharing DTOs -------------------------------------------------------------

type createShareLinkRequest struct {
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	Password     string  `json:"password,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	MaxDownloads *int    `json:"max_downloads,omitempty"`
}

type resolveShareLinkRequest struct {
	Password string `json:"password,omitempty"`
}

type createGuestInviteRequest struct {
	Email     string  `json:"email"`
	FolderID  string  `json:"folder_id"`
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// CreateShareLink creates a share link on a folder or file owned by the
// authenticated workspace. Admin-only per ARCHITECTURE.md §7.3 —
// sharing configuration is a sensitive operation.
func (h *Handler) CreateShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createShareLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if !sharing.IsValidResourceType(req.ResourceType) {
		http.Error(w, "invalid resource_type", http.StatusBadRequest)
		return
	}
	resourceID, err := uuid.Parse(req.ResourceID)
	if err != nil {
		http.Error(w, "invalid resource_id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, req.ResourceType, resourceID); err != nil {
		writeServiceError(w, err)
		return
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
	link, err := h.sharing.CreateShareLink(r.Context(), workspaceID, req.ResourceType, resourceID, req.Password, expiresAt, req.MaxDownloads, userID)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	h.notify(r.Context(), func(n *notification.Service) error {
		return n.NotifyShareLinkCreated(r.Context(), workspaceID, userID, link.ID, req.ResourceType, resourceID)
	})
	writeJSON(w, http.StatusCreated, link)
}

// ResolveShareLink is the public (no-auth) endpoint used by anyone who
// holds a share-link token. When the link is password-protected the
// caller should supply the password in the JSON body of a POST; GET
// without a body is accepted for unprotected links so shares work from
// a plain browser address bar.
func (h *Handler) ResolveShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	token := chi.URLParam(r, "token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	var password string
	if r.Method == http.MethodPost && r.ContentLength > 0 {
		var req resolveShareLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		password = req.Password
	}
	link, err := h.sharing.ResolveShareLink(r.Context(), token, password)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, link)
}

// RevokeShareLink deletes a share link. Admin-only.
func (h *Handler) RevokeShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
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
	if err := h.sharing.RevokeShareLink(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateGuestInvite creates a guest invite and the matching permission
// grant on the target folder. Admin-only.
func (h *Handler) CreateGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createGuestInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	folderID, err := uuid.Parse(req.FolderID)
	if err != nil {
		http.Error(w, "invalid folder_id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, permission.ResourceFolder, folderID); err != nil {
		writeServiceError(w, err)
		return
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
	inv, err := h.sharing.CreateGuestInvite(r.Context(), workspaceID, req.Email, folderID, req.Role, expiresAt, userID)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	// Best-effort invitee notification: only users who already have an
	// account in this workspace get an in-app notification. External
	// invitees receive an email out-of-band (out of scope for this
	// sprint).
	if h.users != nil && h.notifications != nil {
		if u, uerr := h.users.GetByEmail(r.Context(), workspaceID, req.Email); uerr == nil && u != nil {
			h.notify(r.Context(), func(n *notification.Service) error {
				return n.NotifyGuestInviteSent(r.Context(), workspaceID, u.ID, inv.ID, folderID, req.Email)
			})
		}
	}
	writeJSON(w, http.StatusCreated, inv)
}

// AcceptGuestInvite marks an invite accepted. Authenticated endpoint —
// the caller must already hold a token bound to the invite's workspace
// (in practice the token is issued by the invite-acceptance flow).
func (h *Handler) AcceptGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	inv, err := h.sharing.AcceptGuestInvite(r.Context(), workspaceID, id)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	h.notify(r.Context(), func(n *notification.Service) error {
		return n.NotifyGuestInviteAccepted(r.Context(), workspaceID, inv.CreatedBy, inv.ID, inv.Email)
	})
	writeJSON(w, http.StatusOK, inv)
}

// RevokeGuestInvite deletes an invite. Admin-only.
func (h *Handler) RevokeGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
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
	if err := h.sharing.RevokeGuestInvite(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSharingError maps sharing-service errors to HTTP responses.
// ErrNotFound becomes 404, invalid-input errors become 400, expired /
// exhausted / password-related errors become 410 / 429 / 401 / 403 so
// clients can distinguish them without parsing the error text.
func writeSharingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sharing.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, sharing.ErrInvalidResourceType),
		errors.Is(err, sharing.ErrInvalidRole),
		errors.Is(err, sharing.ErrInvalidEmail):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, sharing.ErrLinkExpired),
		errors.Is(err, sharing.ErrInviteExpired):
		http.Error(w, err.Error(), http.StatusGone)
	case errors.Is(err, sharing.ErrLinkExhausted):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, sharing.ErrPasswordRequired):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, sharing.ErrPasswordIncorrect):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, sharing.ErrInviteAlreadyUsed):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		writeServiceError(w, err)
	}
}
