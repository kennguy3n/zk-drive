package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
