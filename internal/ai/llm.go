package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LLMClient is the small surface SummaryService needs from a local
// language model: take a prompt, return a single completion string.
// The interface is deliberately narrow so future swaps (e.g. a
// llama.cpp HTTP server, an in-process binding) can drop in without
// touching the summary pipeline.
//
// Implementations MUST NOT exfiltrate the prompt to a third-party
// service. zk-drive's privacy posture forbids sending file or chat
// content to an external API regardless of operator opt-in. The
// constructors in this package enforce that the configured endpoint
// is loopback (or an explicitly opt-in private address) — see
// NewOllamaClient.
type LLMClient interface {
	// Generate returns the completion for prompt. ctx controls the
	// per-request deadline. Implementations should treat any
	// error (network, decode, model unavailable) as a fall-back
	// signal — SummaryService will silently degrade to its
	// rule-based scaffold rather than fail the HTTP request.
	Generate(ctx context.Context, prompt string) (string, error)

	// Model returns the model identifier the client is configured
	// against (e.g. "qwen2.5:1.5b"). Used for log lines + audit so
	// operators can tell which on-device model produced a summary.
	Model() string
}

// ErrLLMRefusedNonLocal is returned by NewOllamaClient when the
// configured endpoint is not a loopback / private-network address.
// zk-drive's threat model treats LLM-side prompts as plaintext
// content; sending them to a hostname that resolves outside the
// operator's network would silently undo the zero-knowledge contract
// the rest of the product enforces.
var ErrLLMRefusedNonLocal = errors.New("ai: LLM endpoint must be loopback or RFC1918 — refusing to send prompt off-box")

// DefaultOllamaURL is the address the Ollama daemon binds to in
// every supported deployment shape (single-host docker-compose,
// k8s sidecar, and dev laptop). Operators can override via
// OLLAMA_URL when they run the model on a different port.
const DefaultOllamaURL = "http://127.0.0.1:11434"

// DefaultOllamaModel is the on-device model the operator is
// expected to have pulled (`ollama pull qwen2.5:1.5b`). Picked for
// its sub-2 GB memory footprint and the Apache 2.0 weights from
// Alibaba's Qwen team — small enough to colocate with the Go server
// on a 4 GB VPS while still producing usable thread summaries.
//
// Override at runtime with OLLAMA_MODEL when validating a different
// open-weights model. Do NOT change the default to a model whose
// weights are gated behind external API calls.
const DefaultOllamaModel = "qwen2.5:1.5b"

// OllamaClient calls a locally-running Ollama daemon over HTTP. The
// Ollama project (https://ollama.com) wraps llama.cpp with a tiny
// REST server — `POST /api/generate` accepts a model + prompt and
// returns the completion. We deliberately use the non-streaming
// path so the SummaryService can swap the result in atomically.
type OllamaClient struct {
	endpoint string
	model    string
	httpc    *http.Client
}

// NewOllamaClient constructs an OllamaClient against endpoint. An
// empty endpoint resolves to DefaultOllamaURL; an empty model
// resolves to DefaultOllamaModel. The endpoint host MUST be a
// loopback or RFC1918 address — anything else returns
// ErrLLMRefusedNonLocal so the privacy posture is enforced at boot
// rather than per-request. Operators that need to talk to a model
// daemon on another host inside their VPC should use the host's
// private IP (or set up an SSH tunnel to 127.0.0.1).
func NewOllamaClient(endpoint, model string) (*OllamaClient, error) {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultOllamaURL
	}
	if strings.TrimSpace(model) == "" {
		model = DefaultOllamaModel
	}
	if err := assertLocalEndpoint(endpoint); err != nil {
		return nil, err
	}
	return &OllamaClient{
		endpoint: strings.TrimRight(endpoint, "/"),
		model:    model,
		// Generation can be slow on a small CPU-only host —
		// budget 30 s per request and let the caller's context
		// shorten it when the HTTP request itself has a deadline.
		httpc: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Model returns the Ollama model tag this client posts to.
func (c *OllamaClient) Model() string { return c.model }

// ollamaGenerateRequest matches Ollama's POST /api/generate body.
// Stream is forced to false so we get a single JSON document back
// rather than a sequence of deltas — the SummaryService never wants
// partial output.
type ollamaGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// ollamaGenerateResponse matches the non-streaming response shape.
// Done is true on the final (and only) chunk in non-streaming mode.
// If a misconfigured daemon ignores Stream:false and returns NDJSON
// instead, json.Decode here only consumes the first chunk — which
// has Done:false and a partial Response. Generate explicitly
// rejects that case so a half-finished summary never reaches the
// caller.
type ollamaGenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// Generate posts prompt to the Ollama daemon and returns the model's
// completion. Any non-200 response, transport error, or decode
// failure is wrapped and returned so the caller can log + fall
// back; we never panic on bad LLM output.
func (c *OllamaClient) Generate(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(ollamaGenerateRequest{
		Model:  c.model,
		Prompt: prompt,
		Stream: false,
	})
	if err != nil {
		return "", fmt.Errorf("ai/ollama: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ai/ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai/ollama: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read at most 1 KiB of the error body so operator
		// debug logs aren't flooded by a misbehaving daemon.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("ai/ollama: status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var decoded ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("ai/ollama: decode response: %w", err)
	}
	if !decoded.Done {
		// Daemon returned a streaming chunk despite Stream:false.
		// Treat as an error so SummaryService falls back to the
		// scaffold rather than surfacing a partial summary.
		return "", errors.New("ai/ollama: response has done=false (possible streaming misconfiguration)")
	}
	out := strings.TrimSpace(decoded.Response)
	if out == "" {
		return "", errors.New("ai/ollama: empty response")
	}
	return out, nil
}

// assertLocalEndpoint refuses any URL whose host resolves outside
// loopback or RFC1918 / RFC4193 / link-local space. The check is
// deliberately conservative: hostnames that don't resolve at config
// load (because the daemon hasn't booted yet, etc.) fall through
// only when they parse to a literal IP — symbolic hostnames must be
// "localhost" or end in ".local" / ".internal" so accidental DNS
// configurations can't push prompts to a public host.
func assertLocalEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("ai: parse llm endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("ai: llm endpoint scheme %q not supported (want http/https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("ai: llm endpoint has no host")
	}
	// Symbolic hostnames: only allow names that obviously refer to
	// the local box or an operator-controlled internal zone.
	ip := net.ParseIP(host)
	if ip == nil {
		lower := strings.ToLower(host)
		if lower == "localhost" ||
			strings.HasSuffix(lower, ".local") ||
			strings.HasSuffix(lower, ".internal") ||
			strings.HasSuffix(lower, ".cluster.local") {
			return nil
		}
		return fmt.Errorf("%w: host %q is not loopback / .local / .internal", ErrLLMRefusedNonLocal, host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return nil
	}
	return fmt.Errorf("%w: host %s is publicly routable", ErrLLMRefusedNonLocal, ip.String())
}

// BuildSummaryPrompt is the prompt template SummaryService passes to
// the LLM. Exposed so tests can pin the wording and operators can
// see exactly what context their on-device model receives — the
// privacy story depends on this being inspectable, not buried in
// service.go.
func BuildSummaryPrompt(folderName string, fileNames []string, contentPreview string) string {
	var b strings.Builder
	b.WriteString("You are summarising a private team workspace folder for a busy admin. ")
	b.WriteString("Produce a single short paragraph (3–4 sentences) that lists the broad themes, ")
	b.WriteString("not individual file names verbatim. Do not invent files. Do not request more data.\n\n")
	if folderName != "" {
		b.WriteString("Folder name: ")
		b.WriteString(folderName)
		b.WriteString("\n")
	}
	if len(fileNames) > 0 {
		b.WriteString("Files in folder: ")
		b.WriteString(strings.Join(fileNames, ", "))
		b.WriteString("\n")
	}
	if contentPreview != "" {
		b.WriteString("Sample content from files:\n")
		b.WriteString(contentPreview)
		b.WriteString("\n")
	}
	b.WriteString("\nSummary:")
	return b.String()
}
