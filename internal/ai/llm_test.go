package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOllamaClientGenerateSuccess verifies the happy path: the
// client posts a JSON body matching the Ollama /api/generate
// contract and returns the response field verbatim.
func TestOllamaClientGenerateSuccess(t *testing.T) {
	t.Parallel()
	const want = "Acme Q3 marketing planning, including launch deck drafts and budget review."

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		var req ollamaGenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "qwen2.5:1.5b" {
			t.Errorf("model: got %q, want %q", req.Model, "qwen2.5:1.5b")
		}
		if req.Stream {
			t.Errorf("expected stream=false, got true")
		}
		if !strings.Contains(req.Prompt, "Folder name:") {
			t.Errorf("prompt missing folder context: %q", req.Prompt)
		}
		_ = json.NewEncoder(w).Encode(ollamaGenerateResponse{Response: want, Done: true})
	}))
	defer srv.Close()

	client, err := NewOllamaClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewOllamaClient: %v", err)
	}
	got, err := client.Generate(context.Background(),
		BuildSummaryPrompt("Marketing", []string{"q3-launch.md"}, "Q3 launch plan"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != want {
		t.Errorf("Generate: got %q, want %q", got, want)
	}
	if client.Model() != DefaultOllamaModel {
		t.Errorf("Model(): got %q, want %q", client.Model(), DefaultOllamaModel)
	}
}

// TestOllamaClientGenerate5xxIsError confirms a 500 response from
// the daemon surfaces as an error so SummaryService can fall back
// to its scaffold rather than relay garbage to the user.
func TestOllamaClientGenerate5xxIsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model qwen2.5:1.5b not found", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := NewOllamaClient(srv.URL, "qwen2.5:1.5b")
	if err != nil {
		t.Fatalf("NewOllamaClient: %v", err)
	}
	_, err = client.Generate(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

// TestOllamaClientGenerateEmptyResponseIsError ensures a 200 with
// an empty completion is not silently passed through — the caller
// would think the LLM agreed there was nothing to say.
func TestOllamaClientGenerateEmptyResponseIsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaGenerateResponse{Response: "   ", Done: true})
	}))
	defer srv.Close()

	client, err := NewOllamaClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewOllamaClient: %v", err)
	}
	_, err = client.Generate(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected error on empty response, got nil")
	}
}

// TestNewOllamaClientRejectsPublicEndpoint locks in the privacy
// guardrail: configuring the daemon URL to a public hostname or
// public IP must fail at construction, not at request time. Without
// this, an operator could accidentally route plaintext file content
// to a third-party model.
func TestNewOllamaClientRejectsPublicEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"public hostname", "http://api.openai.com:443"},
		{"google AI", "http://generativelanguage.googleapis.com"},
		{"public IP", "http://8.8.8.8:11434"},
		{"https public", "https://example.com/api/generate"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewOllamaClient(tc.url, "")
			if err == nil {
				t.Fatalf("expected ErrLLMRefusedNonLocal for %s, got nil", tc.url)
			}
			if !errors.Is(err, ErrLLMRefusedNonLocal) {
				t.Errorf("expected ErrLLMRefusedNonLocal, got %v", err)
			}
		})
	}
}

// TestNewOllamaClientAcceptsLocalEndpoints sanity-checks that the
// loopback / private / link-local / .local / .internal / .cluster.local
// hostnames all parse without error.
func TestNewOllamaClientAcceptsLocalEndpoints(t *testing.T) {
	t.Parallel()
	cases := []string{
		"http://127.0.0.1:11434",
		"http://localhost:11434",
		"http://10.0.0.5:11434",       // RFC1918
		"http://192.168.1.7:11434",    // RFC1918
		"http://172.16.4.1:11434",     // RFC1918
		"http://[::1]:11434",          // IPv6 loopback
		"http://ollama.internal",      // operator-private DNS
		"http://ollama.local",         // mDNS
		"http://ollama.cluster.local", // k8s DNS
	}
	for _, raw := range cases {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := NewOllamaClient(raw, "qwen2.5:1.5b"); err != nil {
				t.Errorf("expected %s to be accepted, got %v", raw, err)
			}
		})
	}
}

// TestBuildSummaryPromptShape pins the prompt template so future
// edits don't accidentally drop the privacy-relevant guardrails
// ("Do not invent files. Do not request more data.").
func TestBuildSummaryPromptShape(t *testing.T) {
	t.Parallel()
	prompt := BuildSummaryPrompt("Marketing", []string{"a.md", "b.md"}, "Q3 plan")
	for _, want := range []string{
		"You are summarising a private team workspace folder",
		"Do not invent files",
		"Do not request more data",
		"Folder name: Marketing",
		"a.md, b.md",
		"Sample content",
		"\nSummary:",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: full text:\n%s", want, prompt)
		}
	}
}
