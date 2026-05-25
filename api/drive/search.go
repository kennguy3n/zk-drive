package drive

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

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
		http.Error(w, "search not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
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

	results, err := h.search.Search(r.Context(), workspaceID, query, opts, limit, offset)
	if err != nil {
		if errors.Is(err, search.ErrInvalidQuery) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "search: "+err.Error(), http.StatusInternalServerError)
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
