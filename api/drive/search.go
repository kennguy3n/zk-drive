package drive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/responsecache"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// searchCacheTTL is the freshness window for cached search results. 30s
// matches the spec: long enough to absorb the rapid-fire requests a user
// types into the search box (and the duplicate queries across members of
// a busy workspace), short enough that a newly-indexed file surfaces
// quickly even if the workspace-generation bust is somehow missed.
const searchCacheTTL = 30 * time.Second

// searchCacheKey builds a collision-free cache discriminator from every
// input that changes the result set: the normalised query, the resolved
// FTS language, the fuzzy toggle, and the pagination window. Every
// variable-length, free-form field (query AND language) is length-
// prefixed so no choice of `|`-containing values can alias two distinct
// tuples onto the same key — the encoding is injective. The remaining
// fields are fixed-alphabet (fuzzy is "0"/"1"; limit/offset are decimal
// integers) and cannot contain the separator, so they need no prefix.
// Language is server-controlled today (workspaces.search_language), but
// length-prefixing it removes the latent footgun if it ever becomes
// user-influenced.
func searchCacheKey(query string, opts search.Options, limit, offset int) string {
	fuzzy := "0"
	if opts.FuzzyEnabled {
		fuzzy = "1"
	}
	return fmt.Sprintf("%d:%s|%d:%s|%s|%s|%s",
		len(query), query,
		len(opts.Language), opts.Language,
		fuzzy,
		strconv.Itoa(limit),
		strconv.Itoa(offset),
	)
}

// Search runs a workspace-scoped multilingual search over file +
// folder names, tags, and indexed content. q is required; limit
// defaults to DefaultLimit and is capped at MaxLimit in the service
// layer. fuzzy=true relaxes the trigram similarity threshold so
// single-char typos still surface results.
//
// The FTS dictionary is the workspace's configured search_language
// (workspaces.search_language column, settable via the admin
// endpoint). When the workspace service is wired we look it up per
// request — keeps the JWT small and lets admin changes take effect
// without forcing every user to re-login.
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
	limit, err := parseIntQuery(r, "limit", search.DefaultLimit)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid limit")
		return
	}
	offset, err := parseIntQuery(r, "offset", 0)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid offset")
		return
	}
	// Cap at the handler layer so the response envelope echoes the
	// clamped value back to the client. The service also caps
	// defensively, but echoing the pre-service limit would lie to
	// clients that paginate on len(results) < limit.
	if limit > search.MaxLimit {
		limit = search.MaxLimit
	}
	// Cap offset at the handler layer too so the response envelope
	// echoes the clamped value back to the client. Without this, a
	// caller paginating past MaxOffset would see "offset=999999" in
	// the response while the service silently served page 100 — the
	// next-page calculation `offset + limit` would then march
	// further from reality with every click. Mirroring the cap at
	// the handler keeps the response self-consistent and signals
	// to the client that no further pages exist.
	if offset > search.MaxOffset {
		offset = search.MaxOffset
	}

	opts := search.Options{
		FuzzyEnabled: parseBoolParam(r.URL.Query().Get("fuzzy")),
		// Pre-seed Language with the package default. The
		// response envelope echoes opts.Language back to the
		// client so it can render the active dictionary in the
		// UI; if we left it empty here, the response would say
		// "language": "" while the service internally falls back
		// to workspace.DefaultSearchLanguage (see
		// Options.resolvedLanguage). That asymmetry has bitten
		// integration tests in the past — keep the handler and
		// service agreeing on the same default. The workspace
		// lookup below overrides this when it succeeds.
		Language: workspace.DefaultSearchLanguage,
	}
	// Resolve the workspace's preferred FTS dictionary. A lookup
	// failure here is non-fatal: the service falls back to
	// 'simple' which is correct for every language family. We log
	// the failure so an operator can spot a misconfigured
	// workspace, but search must keep working in degraded mode.
	if h.workspaces != nil {
		lang, err := h.workspaces.GetSearchLanguage(r.Context(), workspaceID)
		if err != nil {
			logging.FromContext(r.Context()).Warn("search: resolve workspace language failed",
				"workspace_id", workspaceID, "err", err)
		} else {
			opts.Language = lang
		}
	}

	// Cache the (workspace, query, opts, page) → results for a short
	// window. Search is workspace-scoped and not per-user filtered, so
	// every member issuing the same query gets the same hits — the cache
	// key needs only the query parameters, not the caller's identity.
	// The 30s TTL bounds staleness for newly-indexed files; the
	// workspace generation counter busts the cache immediately on any
	// workspace mutation (see folder.Service write paths) so deletes /
	// renames never linger behind stale search hits.
	cacheKey := searchCacheKey(query, opts, limit, offset)
	results, err := responsecache.GetOrCompute(r.Context(), h.respCache, workspaceID,
		"search", cacheKey, searchCacheTTL,
		func(ctx context.Context) ([]search.Result, error) {
			return h.search.Search(ctx, workspaceID, query, opts, limit, offset)
		})
	if err != nil {
		if errors.Is(err, search.ErrInvalidQuery) {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
			return
		}
		middleware.RespondInternalError(w, r, "search", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hits":     results,
		"query":    query,
		"limit":    limit,
		"offset":   offset,
		"language": opts.Language,
		"fuzzy":    opts.FuzzyEnabled,
	})
}

// parseBoolParam returns true for "true", "1", "yes" (case
// insensitive). Anything else (including "") is false — matches the
// truthy convention used elsewhere in the API for query-string
// booleans.
func parseBoolParam(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes":
		return true
	}
	return false
}
