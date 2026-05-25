package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/index"
)

// searchHit is the shared response shape every assertion in this
// file consumes. Centralised so the contains helper isn't fighting
// anonymous-struct equality across each test.
type searchHit struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Type string    `json:"type"`
}

type searchResponse struct {
	Hits     []searchHit `json:"hits"`
	Query    string      `json:"query"`
	Language string      `json:"language"`
	Fuzzy    bool        `json:"fuzzy"`
}

func containsHit(hits []searchHit, want uuid.UUID) bool {
	for _, h := range hits {
		if h.ID == want {
			return true
		}
	}
	return false
}

// TestSearch_AccentFold pins the accent-folding contract: a query
// without diacritics must surface files whose names contain
// accented characters, and vice versa. The trigram path is the
// primary backbone here — the 'simple' FTS dictionary tokenises on
// whitespace and would emit "café" verbatim, but pg_trgm + the
// immutable_unaccent expression compares unaccented trigrams so
// either query matches either spelling.
func TestSearch_AccentFold(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	want := createFile(t, env, tok.Token, fold.ID.String(), "café notes.txt", "text/plain")
	createFile(t, env, tok.Token, fold.ID.String(), "unrelated.txt", "text/plain")

	// Query without the accent — must still find the accented file.
	status, raw := env.httpRequest(http.MethodGet, "/api/search?q=cafe", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(raw))
	}
	var resp searchResponse
	env.decodeJSON(raw, &resp)
	if !containsHit(resp.Hits, want.ID) {
		t.Fatalf("accent-fold (cafe → café) missed file %s; got %+v", want.ID, resp.Hits)
	}

	// And the reverse: query WITH the accent — must still hit the
	// accented file (and any unaccented version, if it existed).
	status, raw = env.httpRequest(http.MethodGet, "/api/search?q=caf%C3%A9", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search accented: status=%d", status)
	}
	env.decodeJSON(raw, &resp)
	if !containsHit(resp.Hits, want.ID) {
		t.Fatalf("accented query missed file %s", want.ID)
	}
}

// TestSearch_CJKTrigram exercises the trigram path's CJK fallback.
// The 'simple' FTS dictionary tokenises on whitespace and so emits
// the entire Chinese phrase as a single token — a substring query
// would never match. The trigram path indexes 3-character windows
// of the unaccented string, so a Chinese substring still matches
// the full stored name.
func TestSearch_CJKTrigram(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	// "季度报告" = "Quarterly Report" (no spaces — typical CJK).
	want := createFile(t, env, tok.Token, fold.ID.String(), "季度报告.txt", "text/plain")
	createFile(t, env, tok.Token, fold.ID.String(), "english-only.txt", "text/plain")

	// Substring of the CJK name. The trigram operator should still
	// match because the trigrams of "季度报告" cover those of "季度".
	status, raw := env.httpRequest(http.MethodGet, "/api/search?q=%E5%AD%A3%E5%BA%A6", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(raw))
	}
	var resp searchResponse
	env.decodeJSON(raw, &resp)
	if !containsHit(resp.Hits, want.ID) {
		t.Fatalf("CJK trigram query missed file %s; got %+v", want.ID, resp.Hits)
	}
}

// TestSearch_FuzzyTypo pins the fuzzy-mode contract: a single-char
// typo with ?fuzzy=true must still surface the original file.
func TestSearch_FuzzyTypo(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	want := createFile(t, env, tok.Token, fold.ID.String(), "quarterly-report.txt", "text/plain")

	// "reportt" (extra 't') with fuzzy=true should hit.
	status, raw := env.httpRequest(http.MethodGet, "/api/search?q=reportt&fuzzy=true", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(raw))
	}
	var resp searchResponse
	env.decodeJSON(raw, &resp)
	if !resp.Fuzzy {
		t.Errorf("expected fuzzy=true in response, got false")
	}
	if !containsHit(resp.Hits, want.ID) {
		t.Fatalf("fuzzy typo query missed file %s; got %+v", want.ID, resp.Hits)
	}
}

// TestSearch_EnglishStemming covers the FTS path with the english
// dictionary: an admin sets workspaces.search_language=english,
// the worker indexes "running" in content_text, and a query for
// "runs" must hit via Snowball stemming (both forms stem to "run").
// Note: Snowball is NOT a lemmatizer — "ran" and "running" do NOT
// share a stem under English Snowball, so the test uses a real
// shared-stem pair ("running" ↔ "runs" → "run").
func TestSearch_EnglishStemming(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Flip the workspace dictionary to english via the admin endpoint.
	status, body := env.httpRequest(http.MethodPut, "/api/admin/workspace/search-language", tok.Token, map[string]string{
		"language": "english",
	})
	if status != http.StatusOK {
		t.Fatalf("set search language: status=%d body=%s", status, string(body))
	}

	fold := createFolder(t, env, tok.Token, nil, "Docs")
	// Filename intentionally avoids any form of "ran" so the hit
	// must come from content_text via stemming.
	f := createFile(t, env, tok.Token, fold.ID.String(), "athlete-log.txt", "text/plain")

	// Drive the content_text directly — the worker would normally
	// do this after an upload. PersistContent is the same entry
	// point used by tests/integration/index_pdf_docx_test.go.
	svc := index.NewService(env.pool, env.storage, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.PersistContent(ctx, f.ID, "The athlete was running every morning."); err != nil {
		t.Fatalf("persist content: %v", err)
	}

	// Query for a different inflection; english stemmer should
	// match "running" (both "runs" and "running" stem to "run").
	status, raw := env.httpRequest(http.MethodGet, "/api/search?q=runs", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(raw))
	}
	var resp searchResponse
	env.decodeJSON(raw, &resp)
	if resp.Language != "english" {
		t.Errorf("expected language=english in response, got %q", resp.Language)
	}
	if !containsHit(resp.Hits, f.ID) {
		t.Fatalf("english stemmer query 'runs' missed file with 'running' content; got %+v", resp.Hits)
	}
}

// TestAdminSearchLanguage_Validation pins the 400 contract on the
// admin endpoint. An unknown language must NOT poison the
// workspace column (which would later 500 the search query).
func TestAdminSearchLanguage_Validation(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodPut, "/api/admin/workspace/search-language", tok.Token, map[string]string{
		"language": "klingon",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported language, got %d body=%s", status, string(body))
	}
	if !strings.Contains(string(body), "supported") {
		t.Errorf("expected response to surface the supported allow-list, got %s", string(body))
	}

	// Missing key also rejected.
	status, _ = env.httpRequest(http.MethodPut, "/api/admin/workspace/search-language", tok.Token, map[string]string{})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing language key, got %d", status)
	}
}

// TestAdminSearchLanguage_GetAndSet ensures the GET endpoint
// returns the supported allow-list and the PUT endpoint actually
// persists the change.
func TestAdminSearchLanguage_GetAndSet(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodGet, "/api/admin/workspace/search-language", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get search language: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Language  string   `json:"language"`
		Supported []string `json:"supported"`
	}
	env.decodeJSON(body, &resp)
	if resp.Language != "simple" {
		t.Errorf("expected default language=simple, got %q", resp.Language)
	}
	foundEnglish := false
	for _, lang := range resp.Supported {
		if lang == "english" {
			foundEnglish = true
			break
		}
	}
	if !foundEnglish {
		t.Errorf("expected english in supported list, got %v", resp.Supported)
	}

	// Flip to french.
	status, _ = env.httpRequest(http.MethodPut, "/api/admin/workspace/search-language", tok.Token, map[string]string{
		"language": "french",
	})
	if status != http.StatusOK {
		t.Fatalf("set french: status=%d", status)
	}
	// And confirm it stuck.
	status, body = env.httpRequest(http.MethodGet, "/api/admin/workspace/search-language", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get search language: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &resp)
	if resp.Language != "french" {
		t.Errorf("expected language=french after PUT, got %q", resp.Language)
	}
}
