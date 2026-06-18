package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/index"
)

// TestTagSuggestionsRuleBasedScaffold pins the end-to-end pipeline
// for the /files/{id}/tag-suggestions endpoint without any LLM
// configured. The rule-based scaffold must always succeed and
// return at least the extension-derived doc-type tag — that's the
// "deterministic floor" the SuggestionService contract promises.
// Without this integration test, the handler → service → DB
// pipeline would be covered only by unit tests in internal/ai.
func TestTagSuggestionsRuleBasedScaffold(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	fold := createFolder(t, env, tok.Token, nil, "Docs")
	// Filename is deliberately neutral so the extension-derived
	// tag ("pdf") is what surfaces, not a filename token.
	f := createFile(t, env, tok.Token, fold.ID.String(), "neutral-name.pdf", "application/pdf")

	// Persist content_text directly — production would have the
	// index worker do this after upload, but the AI suggestion
	// service reads content_text out of the files row regardless
	// of how it landed there.
	svc := index.NewService(env.pool, env.storage, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.PersistContent(ctx, f.ID, "marketing launch plan for the quarterly review."); err != nil {
		t.Fatalf("persist content: %v", err)
	}

	status, body := env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String()+"/tag-suggestions", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("tag-suggestions: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Suggestions []string `json:"suggestions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode suggestions: %v", err)
	}
	if len(resp.Suggestions) == 0 {
		t.Fatalf("rule-based scaffold returned zero suggestions; expected at least the extension tag 'pdf'")
	}

	// The extension-derived tag ("pdf") must be present because
	// the file name ends in .pdf and extensionTags maps .pdf →
	// "pdf" in autotag.go.
	var sawPDF bool
	for _, s := range resp.Suggestions {
		if s == "pdf" {
			sawPDF = true
			break
		}
	}
	if !sawPDF {
		t.Errorf("expected extension-derived tag 'pdf' in suggestions, got %v", resp.Suggestions)
	}
}

// TestTagSuggestionsStrictZKReturns409 pins the privacy invariant
// for strict-ZK content: the server has no plaintext to analyse,
// so the endpoint must short-circuit with 409 Conflict (which the
// frontend uses to hide the "Suggest tags" affordance for strict-
// ZK files).
func TestTagSuggestionsStrictZKReturns409(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodPost, "/api/folders", tok.Token, map[string]string{
		"name":            "Vault",
		"encryption_mode": folder.EncryptionStrictZK,
	})
	if status != http.StatusCreated {
		t.Fatalf("create strict-zk folder: status=%d body=%s", status, string(body))
	}
	var vault folder.Folder
	env.decodeJSON(body, &vault)

	f := createFile(t, env, tok.Token, vault.ID.String(), "ciphertext.bin", "application/octet-stream")

	status, body = env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String()+"/tag-suggestions", tok.Token, nil)
	if status != http.StatusConflict {
		t.Fatalf("strict-zk tag-suggestions: expected 409, got status=%d body=%s", status, string(body))
	}
}

// TestSearchExpansionRuleBasedScaffold exercises the /search/expand
// endpoint without any LLM configured. The rule-based pass must
// surface workspace tags whose hyphen-bounded segments match the
// query token.
func TestSearchExpansionRuleBasedScaffold(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "marketing-doc.txt", "text/plain")

	// Seed workspace tag vocabulary with a hyphenated tag that
	// has "marketing" as a bounded segment — the rule-based
	// scoring at ruleBasedExpansion gives this +2 (segment match),
	// putting it above the threshold.
	const seed = "marketing-q4-2024"
	status, body := env.httpRequest(http.MethodPost, "/api/files/"+f.ID.String()+"/tags", tok.Token, map[string]string{
		"tag": seed,
	})
	if status != http.StatusCreated {
		t.Fatalf("seed tag: status=%d body=%s", status, string(body))
	}

	status, body = env.httpRequest(http.MethodGet, "/api/search/expand?q=marketing", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search/expand: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Query    string   `json:"query"`
		Terms    []string `json:"terms"`
		LLMUsed  bool     `json:"llm_used"`
		Language string   `json:"language"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode expand: %v", err)
	}
	if resp.Query != "marketing" {
		t.Errorf("expected query echo='marketing', got %q", resp.Query)
	}
	if resp.LLMUsed {
		t.Errorf("expected llm_used=false (no Ollama configured), got true")
	}

	var sawSeed bool
	for _, term := range resp.Terms {
		if term == seed {
			sawSeed = true
			break
		}
	}
	if !sawSeed {
		t.Errorf("expected expansion to surface seeded tag %q for query 'marketing', got %v", seed, resp.Terms)
	}
}

// TestSearchExpansionRequiresQueryParam pins the 400 path: empty
// q must reject up front (we don't want to round-trip to Postgres
// for an empty query).
func TestSearchExpansionRequiresQueryParam(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, _ := env.httpRequest(http.MethodGet, "/api/search/expand?q=", tok.Token, nil)
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for empty q, got %d", status)
	}

	// Missing q entirely also rejects.
	status, _ = env.httpRequest(http.MethodGet, "/api/search/expand", tok.Token, nil)
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for missing q, got %d", status)
	}
}

// TestTagSuggestionsLocalisesPromptByWorkspaceSearchLanguage
// completes the multilingual coverage matrix: in addition to
// the summary endpoint locked in by
// TestThreadSummaryLocalisesPromptByWorkspaceSearchLanguage, the
// tag-suggestion endpoint must also localise its prompt by
// workspace.SearchLanguage. The integration harness now wires
// WithLanguageResolver on tagSuggestSvc (mirroring production
// wiring at cmd/server/main.go:629), so this test confirms the
// resolver is actually consulted at request time.
func TestTagSuggestionsLocalisesPromptByWorkspaceSearchLanguage(t *testing.T) {
	// Capture every prompt the fake daemon receives. Reuses the
	// promptCapturingOllamaServer helper defined in
	// ai_summary_local_llm_test.go so the two AI-multilingual
	// tests share the same capture machinery.
	const llmOutput = "marketing-launch\nq4-strategy\nlancement"
	captured := newPromptCapturingOllamaServer(t, llmOutput)

	t.Setenv("OLLAMA_URL", captured.URL)
	t.Setenv("OLLAMA_MODEL", "qwen2.5:1.5b")

	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Flip workspace dictionary to French via the admin endpoint
	// — same production knob the summary multilingual test uses.
	status, body := env.httpRequest(http.MethodPut, "/api/admin/workspace/search-language", tok.Token, map[string]string{
		"language": "french",
	})
	if status != http.StatusOK {
		t.Fatalf("set french: status=%d body=%s", status, string(body))
	}

	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "plan-lancement.txt", "text/plain")

	// Drive content_text so the LLM stage actually triggers
	// (the rule-based scaffold runs without it, but the LLM
	// path only fires when there's content to feed the prompt).
	svc := index.NewService(env.pool, env.storage, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.PersistContent(ctx, f.ID, "Plan de lancement marketing pour le quatrième trimestre."); err != nil {
		t.Fatalf("persist content: %v", err)
	}

	status, body = env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String()+"/tag-suggestions", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("tag-suggestions: status=%d body=%s", status, string(body))
	}

	// The captured prompt must show the French instruction
	// landed in the system half. We check the LAST prompt
	// (the LLM call) and not the rule-based scaffold path
	// (which doesn't touch the Ollama daemon at all).
	prompts := captured.prompts()
	if len(prompts) == 0 {
		t.Fatalf("expected at least one prompt captured by fake Ollama, got 0")
	}
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "Répondez en français.") {
		t.Errorf("expected last tag-suggest prompt to contain 'Répondez en français.', got:\n%s", last)
	}
	if strings.Contains(last, "Answer in English.") {
		t.Errorf("expected French tag-suggest prompt to NOT contain 'Answer in English.', got:\n%s", last)
	}
}


