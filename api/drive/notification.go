package drive

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/notification"
)

// ListNotifications returns the caller's notifications (unread-first,
// newest-first). limit is capped at 100.
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		http.Error(w, "notifications not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	limit := parseIntParam(r.URL.Query().Get("limit"), notification.DefaultLimit)
	offset := parseIntParam(r.URL.Query().Get("offset"), 0)
	// Cap at the handler layer so the response envelope echoes the
	// clamped values back to clients. The service also clamps
	// defensively, but echoing the pre-service limit would lie to
	// callers that paginate on len(items) < limit.
	if limit <= 0 {
		limit = notification.DefaultLimit
	}
	if limit > notification.MaxLimit {
		limit = notification.MaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	items, err := h.notifications.List(r.Context(), workspaceID, userID, limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notifications": items,
		"limit":         limit,
		"offset":        offset,
	})
}

// MarkNotificationRead flips a single notification to read.
func (h *Handler) MarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		http.Error(w, "notifications not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.notifications.MarkRead(r.Context(), workspaceID, userID, id); err != nil {
		if errors.Is(err, notification.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MarkAllNotificationsRead flips every unread notification for the caller.
func (h *Handler) MarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		http.Error(w, "notifications not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	if err := h.notifications.MarkAllRead(r.Context(), workspaceID, userID); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
