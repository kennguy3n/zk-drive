package drive

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/ai"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// SuggestFileTags returns up to a small number of AI-suggested tags
// for a file based on its name + extracted content. The suggestions
// are advisory — selecting one calls back through the normal
// AddFileTag handler, so an LLM never writes tags directly. Editor
// permission on the file is required so a viewer can't probe a
// file's AI signal as a side channel.
//
// Endpoint semantics:
//   - 200 with {suggestions: []} on success (may be empty list for
//     a file with no extracted content + no overlapping tags).
//   - 404 if the file doesn't exist in the workspace.
//   - 409 if the file lives in a strict-ZK folder (server has no
//     plaintext to analyse). The frontend uses this to hide the
//     "Suggest tags" affordance for strict-ZK content.
//   - 501 if the suggestion service hasn't been wired (no LLM
//     daemon + no rule-based-only deployment configured).
func (h *Handler) SuggestFileTags(w http.ResponseWriter, r *http.Request) {
	if h.tagSuggest == nil {
		http.Error(w, "tag suggestions not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
		writeServiceError(w, r, err)
		return
	}
	suggestions, err := h.tagSuggest.Suggest(r.Context(), workspaceID, id)
	if err != nil {
		writeTagSuggestError(w, err)
		return
	}
	// Defense-in-depth: the production SuggestionService.Suggest
	// always returns a non-nil slice (via make([]string, 0, ...)
	// in ruleBasedSuggestions), but a third-party TagSuggester
	// implementation could legally return (nil, nil), which would
	// serialise as {"suggestions": null} instead of the documented
	// {"suggestions": []}. Same rationale as the ExpandResult nil-
	// coalesce at internal/ai/queryexp.go:194-196. Devin Review
	// ANALYSIS_0003 on commit b4b41dd.
	if suggestions == nil {
		suggestions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"suggestions": suggestions,
	})
}

// ExpandSearchQuery returns multilingual-aware expansion terms for
// a search query. The expansion list is rule-based (workspace-tag
// overlap) and, when an on-device LLM is configured, refined with
// synonyms in the workspace's preferred FTS language. The frontend
// renders the result as a row of suggested-search chips next to the
// search bar — selecting one re-issues /api/search with the
// expanded term.
//
// Endpoint semantics:
//   - 200 with {terms: []} on success. terms may be empty for a
//     query that has no overlap with the workspace tag vocabulary
//     and no LLM was wired.
//   - 400 if q is missing or empty.
//   - 501 if the expansion service hasn't been wired.
func (h *Handler) ExpandSearchQuery(w http.ResponseWriter, r *http.Request) {
	if h.queryExpand == nil {
		http.Error(w, "query expansion not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	terms, llmUsed, language, err := h.queryExpand.Expand(r.Context(), workspaceID, query)
	if err != nil {
		http.Error(w, "expand: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Defense-in-depth: same rationale as SuggestFileTags above. The
	// production ExpansionService.ExpandResult coalesces nil internally
	// (at internal/ai/queryexp.go:194-196), but the handler operates on
	// the QueryExpander interface — a third-party implementation could
	// return (nil, false, "", nil) and we'd serialise `"terms": null`
	// instead of the documented `"terms": []`. Both sibling handlers in
	// this file now apply the same guard. Devin Review BUG_0001 on
	// commit 744909a.
	if terms == nil {
		terms = []string{}
	}
	// Coalesce empty language to the package default for response-
	// shape consistency with the sibling /api/search handler
	// (api/drive/search.go:69 pre-seeds opts.Language with
	// workspace.DefaultSearchLanguage "simple" before resolving the
	// workspace's stored language, so its JSON response is never
	// "language": ""). The internal ai.resolveWorkspaceLanguage
	// helper returns "" on a nil resolver or a transient lookup
	// failure — appropriate at the prompt-builder boundary because
	// PromptLanguageFor("") falls through to the English fallback,
	// but inappropriate at the JSON-response boundary because third-
	// party API consumers expect a stable language token. Aligning
	// here keeps the two endpoints' "language" semantics identical
	// without changing the prompt-resolution path. Devin Review
	// ANALYSIS_0002 on commit de78db5.
	if language == "" {
		language = workspace.DefaultSearchLanguage
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":    query,
		"terms":    terms,
		"llm_used": llmUsed,
		"language": language,
	})
}

// writeTagSuggestError maps suggestion-service errors to HTTP
// statuses. The strict-ZK case is the most user-facing — the
// frontend uses 409 specifically to know to hide the affordance,
// not to surface a generic error toast. We match via errors.Is on
// the typed sentinels (ai.ErrTagSuggest*) rather than string
// matching so the contract is checked by the compiler when the ai
// package renames a sentinel.
func writeTagSuggestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ai.ErrTagSuggestUnavailable):
		http.Error(w, "tag suggestions unavailable for strict-zk content", http.StatusConflict)
	case errors.Is(err, ai.ErrTagSuggestFileNotFound):
		http.Error(w, "file not found", http.StatusNotFound)
	default:
		http.Error(w, "suggest: "+err.Error(), http.StatusInternalServerError)
	}
}
