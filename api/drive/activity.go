package drive

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
)

// activityMaxPageSize must stay in sync with activity.normalizePaging so
// the handler can echo the effective limit back to the client without
// lying about how many rows the repository actually returned.
const activityMaxPageSize = 200

// ListActivity returns paginated activity_log entries for the authenticated
// workspace. If workspace_id query param is provided it must match the
// tenant bound to the session.
func (h *Handler) ListActivity(w http.ResponseWriter, r *http.Request) {
	if h.activity == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "activity not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	if wsParam := r.URL.Query().Get("workspace_id"); wsParam != "" {
		wsID, err := uuid.Parse(wsParam)
		if err != nil || wsID != workspaceID {
			middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeWrongTenant, "workspace_id mismatch")
			return
		}
	}
	limit := parseIntParam(r.URL.Query().Get("limit"), 50)
	offset := parseIntParam(r.URL.Query().Get("offset"), 0)
	// Keep in sync with activity.normalizePaging — the handler echoes the
	// effective limit back to the client, so a client doing
	// len(entries) < limit to detect end-of-stream stays correct when the
	// repo silently caps oversized requests.
	if limit <= 0 {
		limit = 50
	}
	if limit > activityMaxPageSize {
		limit = activityMaxPageSize
	}

	var (
		list []*activity.LogEntry
		err  error
	)
	if rt, rid := r.URL.Query().Get("resource_type"), r.URL.Query().Get("resource_id"); rt != "" && rid != "" {
		resourceID, perr := uuid.Parse(rid)
		if perr != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid resource_id")
			return
		}
		list, err = h.activity.ListByResource(r.Context(), workspaceID, rt, resourceID, limit, offset)
	} else {
		list, err = h.activity.List(r.Context(), workspaceID, limit, offset)
	}
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "list activity: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": list,
		"limit":   limit,
		"offset":  offset,
	})
}
