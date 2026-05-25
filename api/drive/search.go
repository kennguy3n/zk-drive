package drive

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/search"
)

// Search runs a workspace-scoped FTS query over file + folder names and
// returns results ranked by ts_rank_cd. q is required; limit defaults
// to DefaultLimit and is capped at MaxLimit in the service layer.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	if h.search == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "search not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "q is required")
		return
	}
	limit := parseIntParam(r.URL.Query().Get("limit"), search.DefaultLimit)
	offset := parseIntParam(r.URL.Query().Get("offset"), 0)
	// Cap at the handler layer so the response envelope echoes the
	// clamped value back to the client. The service also caps
	// defensively, but echoing the pre-service limit would lie to
	// clients that paginate on len(results) < limit.
	if limit > search.MaxLimit {
		limit = search.MaxLimit
	}
	results, err := h.search.Search(r.Context(), workspaceID, query, limit, offset)
	if err != nil {
		if errors.Is(err, search.ErrInvalidQuery) {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "search: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hits":   results,
		"query":  query,
		"limit":  limit,
		"offset": offset,
	})
}
