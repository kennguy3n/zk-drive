package drive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// stubExpander is a minimal QueryExpander used by the handler tests
// below. It returns whatever the test sets up — including the
// (terms=nil, language="") edge case that the handler's response-
// shape guards (terms nil-coalesce, language default-coalesce) are
// supposed to normalise away.
type stubExpander struct {
	terms    []string
	llmUsed  bool
	language string
	err      error
}

func (s *stubExpander) Expand(ctx context.Context, workspaceID uuid.UUID, query string) ([]string, bool, string, error) {
	return s.terms, s.llmUsed, s.language, s.err
}

// TestExpandSearchQueryCoalescesEmptyLanguage pins the response-
// shape guard. The /api/search/expand endpoint must serialise
// "language": "simple" (or whatever workspace.DefaultSearchLanguage
// is at the time) instead of "language": "" when the upstream
// ExpansionService resolver returned an empty string (nil resolver,
// transient lookup failure). This matches /api/search at
// api/drive/search.go:69 which pre-seeds opts.Language with the
// same default before resolution.
func TestExpandSearchQueryCoalescesEmptyLanguage(t *testing.T) {
	h := &Handler{}
	h.WithQueryExpander(&stubExpander{terms: []string{"marketing-q4-2024"}, language: ""})

	wsID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/search/expand?q=q4", nil)
	req = req.WithContext(middleware.WithWorkspaceID(req.Context(), wsID))
	w := httptest.NewRecorder()
	h.ExpandSearchQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Query    string   `json:"query"`
		Terms    []string `json:"terms"`
		LLMUsed  bool     `json:"llm_used"`
		Language string   `json:"language"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Language != workspace.DefaultSearchLanguage {
		t.Errorf("language = %q, want %q (the workspace package default — handler must coalesce empty resolver output)",
			resp.Language, workspace.DefaultSearchLanguage)
	}
}

// TestExpandSearchQueryPreservesResolvedLanguage pins that the
// default-coalesce does NOT clobber a successfully-resolved
// non-empty language. The coalesce only fires on "" — if the
// resolver returned "german", the handler must echo that back
// verbatim so the frontend can render the active dictionary in the
// UI.
func TestExpandSearchQueryPreservesResolvedLanguage(t *testing.T) {
	h := &Handler{}
	h.WithQueryExpander(&stubExpander{terms: []string{"marketing"}, language: "german"})

	wsID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/search/expand?q=marketing", nil)
	req = req.WithContext(middleware.WithWorkspaceID(req.Context(), wsID))
	w := httptest.NewRecorder()
	h.ExpandSearchQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Language string `json:"language"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Language != "german" {
		t.Errorf("language = %q, want %q (handler must echo resolved language verbatim)", resp.Language, "german")
	}
}

// TestExpandSearchQueryCoalescesNilTerms pins the terms nil-
// coalesce guard.
// A third-party QueryExpander implementation can legally return
// (nil, false, "", nil) and the handler must serialise
// "terms": [] not "terms": null — same JSON-shape contract third-
// party API consumers rely on.
func TestExpandSearchQueryCoalescesNilTerms(t *testing.T) {
	h := &Handler{}
	h.WithQueryExpander(&stubExpander{terms: nil, language: workspace.DefaultSearchLanguage})

	wsID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/search/expand?q=anything", nil)
	req = req.WithContext(middleware.WithWorkspaceID(req.Context(), wsID))
	w := httptest.NewRecorder()
	h.ExpandSearchQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Inspect the raw JSON to assert "terms": [] rather than
	// "terms": null. Unmarshalling into []string accepts both.
	body := w.Body.String()
	if !contains(body, `"terms":[]`) {
		t.Errorf("body should contain `\"terms\":[]`, got %s", body)
	}
}

// contains is a small local helper to keep the test file from
// importing strings just for one check.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
