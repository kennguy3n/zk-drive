package integration

import (
	"net/http"
	"strings"
	"testing"
)

// TestSearchByName exercises the happy path: create a handful of files
// in the workspace and verify that GET /api/search?q=... returns rows
// matching the query envelope agreed with the frontend
// (SearchResponse = { hits, query, limit, offset }).
func TestSearchByName(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	createFile(t, env, tok.Token, fold.ID.String(), "quarterly report.pdf", "application/pdf")
	createFile(t, env, tok.Token, fold.ID.String(), "invoice document.pdf", "application/pdf")
	createFile(t, env, tok.Token, fold.ID.String(), "design notes.md", "text/markdown")

	status, body := env.httpRequest(http.MethodGet, "/api/search?q=invoice", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
		Hits   []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"hits"`
	}
	env.decodeJSON(body, &resp)
	if resp.Query != "invoice" {
		t.Errorf("expected echoed query=invoice, got %q", resp.Query)
	}
	if len(resp.Hits) == 0 {
		t.Fatalf("expected at least one hit for invoice, got none")
	}
	found := false
	for _, h := range resp.Hits {
		if strings.Contains(h.Name, "invoice") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a hit whose name contains 'invoice', got %+v", resp.Hits)
	}
}

// TestSearchEmptyQueryRejected pins the 400 contract for empty
// queries; the frontend relies on this to avoid firing a useless
// request on every keystroke while the user clears the search box.
func TestSearchEmptyQueryRejected(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, _ := env.httpRequest(http.MethodGet, "/api/search?q=", tok.Token, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("empty query: expected 400, got %d", status)
	}
}

// TestSearchPagination verifies that limit/offset are echoed back in
// the response envelope and that offset advances the result window.
// Written as a single test because the setup is identical and splitting
// it would triple the fixture cost without improving isolation.
func TestSearchPagination(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Pages")

	for i := 0; i < 3; i++ {
		createFile(t, env, tok.Token, fold.ID.String(),
			[]string{"alpha one page.md", "alpha two page.md", "alpha three page.md"}[i],
			"text/markdown")
	}

	status, body := env.httpRequest(http.MethodGet, "/api/search?q=alpha&limit=2&offset=0", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("page1: status=%d body=%s", status, string(body))
	}
	var page1 struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
		Hits   []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"hits"`
	}
	env.decodeJSON(body, &page1)
	if page1.Limit != 2 || page1.Offset != 0 {
		t.Errorf("expected limit=2 offset=0, got limit=%d offset=%d", page1.Limit, page1.Offset)
	}
	if len(page1.Hits) != 2 {
		t.Errorf("expected 2 hits on page1, got %d", len(page1.Hits))
	}

	status, body = env.httpRequest(http.MethodGet, "/api/search?q=alpha&limit=2&offset=2", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("page2: status=%d body=%s", status, string(body))
	}
	var page2 struct {
		Offset int `json:"offset"`
		Hits   []struct {
			ID string `json:"id"`
		} `json:"hits"`
	}
	env.decodeJSON(body, &page2)
	if page2.Offset != 2 {
		t.Errorf("expected offset=2, got %d", page2.Offset)
	}
	// Page 2 may be empty or contain the remaining row — we assert
	// non-overlap with page 1 rather than a strict count because FTS
	// ranking can put folder rows ahead of file rows depending on
	// dictionaries.
	for _, h1 := range page1.Hits {
		for _, h2 := range page2.Hits {
			if h1.ID == h2.ID {
				t.Errorf("page2 overlapped page1 on id %s", h1.ID)
			}
		}
	}
}
