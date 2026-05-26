package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeOllamaServer mints a httptest.Server that speaks the subset of
// the Ollama /api/generate contract zk-drive depends on. Tests pass
// it the body they want the model to "produce" and we wrap it in the
// {"response": ..., "done": true} envelope our OllamaClient decodes.
//
// The server URL is loopback (httptest.NewServer binds 127.0.0.1) so
// it satisfies the privacy guardrail in NewOllamaClient.
func fakeOllamaServer(t *testing.T, response string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("fake ollama: unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if status != 0 && status != http.StatusOK {
			http.Error(w, "fake ollama upstream error", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": response,
			"done":     true,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// promptCapturingOllamaServer is a fake Ollama daemon that records
// every prompt body it receives. Used to assert the system half of
// the prompt was localised by the workspace.SearchLanguage resolver
// — i.e. that the multilingual codepath is actually live end-to-end
// (handler → SummaryService.resolveLanguage → BuildSummaryPrompt →
// OllamaClient.Generate) and not just unit-tested in isolation.
type promptCapturingOllamaServer struct {
	*httptest.Server
	mu          sync.Mutex
	requestBody []string // raw request bodies in receive order
}

func newPromptCapturingOllamaServer(t *testing.T, response string) *promptCapturingOllamaServer {
	t.Helper()
	c := &promptCapturingOllamaServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("fake ollama: unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake ollama: read body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.requestBody = append(c.requestBody, string(body))
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": response,
			"done":     true,
		})
	}))
	t.Cleanup(c.Server.Close)
	return c
}

func (c *promptCapturingOllamaServer) prompts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.requestBody))
	copy(out, c.requestBody)
	return out
}

// TestThreadSummaryUsesLocalLLMWhenConfigured confirms that with
// OLLAMA_URL pointing at a (fake) local daemon, the /summary
// response is the model's output verbatim — proving the LLM path is
// live, not just compiled in.
func TestThreadSummaryUsesLocalLLMWhenConfigured(t *testing.T) {
	const llmOutput = "Quarterly marketing planning workspace; primarily launch deck drafts and budget review."
	srv := fakeOllamaServer(t, llmOutput, http.StatusOK)

	t.Setenv("OLLAMA_URL", srv.URL)
	t.Setenv("OLLAMA_MODEL", "qwen2.5:1.5b")

	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	const room = "kchat-room-llm"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": room,
	})
	if status != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", status, string(body))
	}
	var created kchatRoomCreated
	env.decodeJSON(body, &created)
	createFile(t, env, tok.Token, created.FolderID.String(), "q3-launch.md", "text/markdown")

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms/"+created.ID.String()+"/summary", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("summary: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if !strings.Contains(resp.Summary, llmOutput) {
		t.Errorf("expected LLM output %q in summary, got %q", llmOutput, resp.Summary)
	}
}

// TestThreadSummaryFallsBackWhenLocalLLMErrors locks in the
// graceful-degrade behaviour: if the daemon 5xx's the user still
// gets a 200 with the deterministic scaffold, not a failure cascade.
func TestThreadSummaryFallsBackWhenLocalLLMErrors(t *testing.T) {
	srv := fakeOllamaServer(t, "", http.StatusInternalServerError)
	t.Setenv("OLLAMA_URL", srv.URL)
	t.Setenv("OLLAMA_MODEL", "qwen2.5:1.5b")

	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	const room = "kchat-room-llm-fallback"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": room,
	})
	if status != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", status, string(body))
	}
	var created kchatRoomCreated
	env.decodeJSON(body, &created)
	createFile(t, env, tok.Token, created.FolderID.String(), "fallback.md", "text/markdown")

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms/"+created.ID.String()+"/summary", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("summary: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	// Scaffold output always names the file count + file names; an
	// empty / model-style answer would not.
	if !strings.Contains(resp.Summary, "fallback.md") {
		t.Errorf("expected scaffold fallback to mention file name, got %q", resp.Summary)
	}
	if !strings.Contains(resp.Summary, "Room contains") {
		t.Errorf("expected scaffold prefix 'Room contains', got %q", resp.Summary)
	}
}

// TestThreadSummaryLocalisesPromptByWorkspaceSearchLanguage proves
// the end-to-end multilingual codepath. The integration harness now
// wires WithLanguageResolver on the summary service (matching
// production at cmd/server/main.go:622), so flipping the workspace's
// search_language to "french" must cause BuildSummaryPrompt to emit
// the French instruction half — and the captured Ollama request body
// must reflect that. Devin Review ANALYSIS_0003 on PR #85 flagged
// that without this test, the multilingual codepath was only
// covered by unit tests in internal/ai/llm_test.go and the
// integration wire-up was effectively dead.
func TestThreadSummaryLocalisesPromptByWorkspaceSearchLanguage(t *testing.T) {
	const llmOutput = "Résumé du salon de discussion."
	srv := newPromptCapturingOllamaServer(t, llmOutput)

	t.Setenv("OLLAMA_URL", srv.URL)
	t.Setenv("OLLAMA_MODEL", "qwen2.5:1.5b")

	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Flip the workspace dictionary to French via the admin
	// endpoint — this is the exact knob production operators
	// use, so the test exercises the same code path.
	status, body := env.httpRequest(http.MethodPut, "/api/admin/workspace/search-language", tok.Token, map[string]string{
		"language": "french",
	})
	if status != http.StatusOK {
		t.Fatalf("set french: status=%d body=%s", status, string(body))
	}

	const room = "kchat-room-multilingual"
	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": room,
	})
	if status != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", status, string(body))
	}
	var created kchatRoomCreated
	env.decodeJSON(body, &created)
	createFile(t, env, tok.Token, created.FolderID.String(), "réunion-q4.md", "text/markdown")

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms/"+created.ID.String()+"/summary", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("summary: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if !strings.Contains(resp.Summary, llmOutput) {
		t.Errorf("expected French model output %q in summary, got %q", llmOutput, resp.Summary)
	}

	// The captured prompt body must show that the resolver
	// correctly pulled "french" from workspaces.search_language
	// and BuildSummaryPrompt swapped in the French instruction.
	// We assert on the exact French phrase emitted by
	// PromptLanguageFor("french") at internal/ai/multilingual.go:77
	// — locking in the wire payload rather than just the in-memory
	// PromptLanguage struct that the llm_test.go unit tests cover.
	prompts := srv.prompts()
	if len(prompts) == 0 {
		t.Fatalf("expected at least one prompt captured by fake Ollama, got 0")
	}
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "Répondez en français.") {
		t.Errorf("expected last prompt to contain 'Répondez en français.', got:\n%s", last)
	}
	// And the English fallback must NOT appear — if it did, the
	// resolver wire-up regressed and the prompt is mistakenly
	// using the English default.
	if strings.Contains(last, "Answer in English.") {
		t.Errorf("expected French prompt to NOT contain 'Answer in English.', got:\n%s", last)
	}
}
