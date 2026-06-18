package drive

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/changefeed"
	"github.com/kennguy3n/zk-drive/internal/logging"
)

// changesResponse is the JSON shape returned by the catch-up
// endpoint. It mirrors changefeed.Page but is duplicated here so
// the wire schema is local to the drive package and any future
// drive-layer envelope (e.g. error wrapping, x-headers) can be
// added without churning the changefeed package.
type changesResponse struct {
	Mutations []changefeed.Mutation `json:"mutations"`
	Cursor    int64                 `json:"cursor"`
	HasMore   bool                  `json:"has_more"`
}

// ListChanges serves GET /api/changes — the cursor-paged catch-up
// stream consumed by the desktop sync SDK. The auth middleware has
// already established the workspace from the JWT, so callers cannot
// query a workspace they aren't part of.
//
// Query parameters:
//   - since: int64 cursor; clients pass the highest sequence they
//     have processed so far. 0 (or unset) returns from the
//     beginning of history.
//   - limit: int page size; clamped to (0, changefeed.MaxLimit].
//     When unset or non-positive, changefeed.DefaultLimit is used.
//
// Response shape:
//
//	{
//	  "mutations": [ ... ],
//	  "cursor":    1234,
//	  "has_more":  true
//	}
//
// The advertised cursor is the sequence of the LAST mutation in the
// page (or the supplied `since` value when the page is empty). When
// has_more is true clients should call again immediately; when
// false they are caught up to "now" and can rely on the WebSocket
// stream for incremental updates.
func (h *Handler) ListChanges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.changefeed == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "change feed not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(ctx)
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	since, err := parseInt64Query(r, "since", 0)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid since cursor")
		return
	}
	limit, err := parseIntQuery(r, "limit", changefeed.DefaultLimit)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid limit")
		return
	}
	page, err := h.changefeed.Since(ctx, workspaceID, since, limit)
	if err != nil {
		logging.FromContext(ctx).Error("changefeed since failed",
			"workspace_id", workspaceID,
			"since", since,
			"limit", limit,
			"err", err,
		)
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, changesResponse{
		Mutations: page.Mutations,
		Cursor:    page.Cursor,
		HasMore:   page.HasMore,
	})
}

// LatestChange serves GET /api/changes/latest — returns the highest
// sequence currently stored for the caller's workspace. Used by sync
// clients on first connect to learn the "now" cursor before
// transitioning into live-stream mode.
//
// Response shape: { "cursor": 1234 }
func (h *Handler) LatestChange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.changefeed == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "change feed not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(ctx)
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	seq, err := h.changefeed.Latest(ctx, workspaceID)
	if err != nil {
		logging.FromContext(ctx).Error("changefeed latest failed",
			"workspace_id", workspaceID,
			"err", err,
		)
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Cursor int64 `json:"cursor"`
	}{Cursor: seq})
}

// parseInt64Query reads a non-negative int64 from r.URL.Query()[name].
// An empty / missing value returns def with no error; a present but
// unparseable value returns an error so the handler can respond 400.
// Negative values are clipped to def so an unset and a deliberately-
// malformed `?since=-1` produce the same observable response — the
// caller picks the "no input" sentinel via def. Currently every call
// site passes def=0 (since/after_seq both want "from the beginning"
// on missing input), but threading def through means a future caller
// like parseInt64Query(r, "cursor", lastCursorSeq) would behave as
// the function name suggests rather than silently snapping to 0.
//
// TrimSpace matches the sibling parseIntQuery below so the two
// helpers handle whitespace identically — a client passing
// `?since=%2050` (url-encoded space + 50) parses cleanly instead
// of returning 400 from one helper and succeeding on the other.
func parseInt64Query(r *http.Request, name string, def int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return def, nil
	}
	return v, nil
}

// parseIntQuery is the int equivalent of parseInt64Query. The narrower
// type matches the changefeed.Since limit parameter which is
// constrained to MaxLimit = 500.
//
// Both helpers are now numerically symmetric: negative values clip to
// `def` in both, matching the admin package's parseIntQuery and the
// "negative-equals-unset" principle of least surprise. The service
// layer also defends with `limit <= 0 -> DefaultLimit`, but clipping
// at the edge means a negative limit and an unset limit produce the
// same observable response. parseInt64Query previously clipped a
// negative value to a hardcoded 0 instead of `def`; both helpers now
// share the same negative-clipping contract.
func parseIntQuery(r *http.Request, name string, def int) (int, error) {
	// TrimSpace matches the admin package's parseIntQuery so a
	// client passing `?limit=%2050` (url-encoded space + 50)
	// resolves identically against both packages.
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return def, nil
	}
	return v, nil
}
