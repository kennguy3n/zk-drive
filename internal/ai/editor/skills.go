// Package editor implements the AI skill service for the collaborative
// document editor. Skills are narrow, single-pass prompts that ask a
// local LLM to perform a specific transformation on a selection of
// document text (e.g. "improve writing", "summarize", "translate").
//
// The service is deliberately simple: each skill is a Go struct that
// builds a prompt from the selection text, sends it to the LLM client's
// streaming endpoint, and returns a channel of tokens for the HTTP
// handler to stream via SSE to the frontend ghost block.
//
// Privacy: the LLM client constructor enforces loopback-only endpoints
// (see internal/ai/llm.go). Strict-ZK folders are rejected at the
// handler level — the service never sees them.
package editor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kennguy3n/zk-drive/internal/ai"
)

// ErrLLMNotConfigured is returned when no LLM client has been wired.
// Handlers map this to 501.
var ErrLLMNotConfigured = errors.New("editor: LLM not configured")

// ErrUnknownSkill is returned when the requested skill ID is not
// registered. Handlers map this to 400.
var ErrUnknownSkill = errors.New("editor: unknown skill")

// ErrEmptySelection is returned when the skill requires a selection
// but none was provided. Handlers map this to 400.
var ErrEmptySelection = errors.New("editor: selection is required for this skill")

// MaxLanguageChars caps the language field length to prevent prompt
// injection via an oversized language parameter.
const MaxLanguageChars = 50

// MaxContextChars caps the document context sent to the LLM. For a
// 4K-context model at ~4 chars/token, this is ~3K tokens of document
// content, leaving ~1K for the skill prompt itself. Larger models
// (8K-16K context) can afford more, but the cap keeps latency bounded
// on consumer hardware.
const MaxContextChars = 12000

// MaxSelectionChars caps the selection text sent to the LLM. Selections
// longer than this are truncated to keep the prompt within the model's
// context window.
const MaxSelectionChars = 8000

// CharsPerToken is a rough heuristic for estimating token count from
// character count. English text averages ~4 chars/token; we use 3.5 to
// be conservative (non-English text tends to have more tokens per char).
const CharsPerToken = 3.5

// EstimateTokens returns a rough token count for a string. Used for
// context budget management — not a precise tokenizer, but sufficient
// for deciding whether to truncate input before sending to the LLM.
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return int(float64(utf8.RuneCountInString(s))/CharsPerToken) + 1
}

// SkillID identifies a specific AI skill. Frontend sends this as a
// string in the request body; the service looks it up in the registry.
type SkillID string

const (
	SkillImproveWriting SkillID = "improve_writing"
	SkillSummarize      SkillID = "summarize"
	SkillExpand         SkillID = "expand"
	SkillSimplify       SkillID = "simplify"
	SkillTranslate      SkillID = "translate"
	SkillGenerateIdeas  SkillID = "generate_ideas"
)

// SkillRequest is the input to a skill execution. Selection is the
// highlighted text (may be empty for generative skills). Context is
// the surrounding document text for additional context (may be empty).
// Language is the workspace's preferred language for the response.
type SkillRequest struct {
	Selection string
	Context   string
	Language  string
}

// Skill defines a single AI skill: its ID, whether it requires a
// selection, and a function that builds the prompt for the LLM.
type Skill struct {
	ID                SkillID
	RequiresSelection bool
	BuildPrompt       func(req SkillRequest) string
	// MaxContextCharsOverride allows a skill to use a smaller context
	// budget than the global MaxContextChars. When 0, the global cap
	// is used. Skills that produce longer output (e.g. expand) can
	// afford less input context; skills that only need a short
	// selection (e.g. summarize) can use the full budget.
	MaxContextCharsOverride int
}

// Registry of all available skills. Maps skill ID to the Skill struct.
var skillRegistry = map[SkillID]Skill{
	SkillImproveWriting: {
		ID:                SkillImproveWriting,
		RequiresSelection: true,
		BuildPrompt:       buildImproveWritingPrompt,
	},
	SkillSummarize: {
		ID:                SkillSummarize,
		RequiresSelection: true,
		BuildPrompt:       buildSummarizePrompt,
	},
	SkillExpand: {
		ID:                SkillExpand,
		RequiresSelection: true,
		BuildPrompt:       buildExpandPrompt,
	},
	SkillSimplify: {
		ID:                SkillSimplify,
		RequiresSelection: true,
		BuildPrompt:       buildSimplifyPrompt,
	},
	SkillTranslate: {
		ID:                SkillTranslate,
		RequiresSelection: true,
		BuildPrompt:       buildTranslatePrompt,
	},
	SkillGenerateIdeas: {
		ID:                SkillGenerateIdeas,
		RequiresSelection: false,
		BuildPrompt:       buildGenerateIdeasPrompt,
	},
}

// AvailableSkills returns all registered skill IDs in a deterministic
// order. Used by the frontend to populate the slash command AI section.
func AvailableSkills() []SkillID {
	return []SkillID{
		SkillImproveWriting,
		SkillSummarize,
		SkillExpand,
		SkillSimplify,
		SkillTranslate,
		SkillGenerateIdeas,
	}
}

// SkillService orchestrates AI skill execution against a local LLM.
type SkillService struct {
	llm ai.LLMClient
}

// NewSkillService creates a SkillService. The LLM client may be nil —
// Execute will return ErrLLMNotConfigured so handlers can map to 501.
func NewSkillService(llm ai.LLMClient) *SkillService {
	return &SkillService{llm: llm}
}

// WithLLM wires (or replaces) the LLM client. Follows the same
// typed-nil guard pattern as the other AI services.
func (s *SkillService) WithLLM(llm ai.LLMClient) *SkillService {
	if llm == nil {
		return s
	}
	s.llm = llm
	return s
}

// Validate checks whether a skill request is valid without executing
// it. Returns the same sentinel errors as Execute so the HTTP handler
// can reject bad requests with a proper status code before opening the
// SSE stream (after WriteHeader(200) the only way to signal an error
// is via an SSE event, which hides the real HTTP status from proxies
// and monitoring).
func (s *SkillService) Validate(skillID SkillID, req SkillRequest) error {
	if s.llm == nil {
		return ErrLLMNotConfigured
	}
	skill, ok := skillRegistry[skillID]
	if !ok {
		return ErrUnknownSkill
	}
	if skill.RequiresSelection && strings.TrimSpace(req.Selection) == "" {
		return ErrEmptySelection
	}
	return nil
}

// Execute runs the named skill on the request and returns channels for
// streaming tokens and errors. The token channel is closed when the
// stream completes; the error channel receives at most one error.
// ctx cancellation aborts the stream.
func (s *SkillService) Execute(ctx context.Context, skillID SkillID, req SkillRequest) (<-chan string, <-chan error) {
	errs := make(chan error, 1)
	tokens := make(chan string, 64)

	go func() {
		defer close(tokens)
		defer close(errs)

		if s.llm == nil {
			errs <- ErrLLMNotConfigured
			return
		}

		skill, ok := skillRegistry[skillID]
		if !ok {
			errs <- ErrUnknownSkill
			return
		}

		if skill.RequiresSelection && strings.TrimSpace(req.Selection) == "" {
			errs <- ErrEmptySelection
			return
		}

		// Truncate selection and context to their respective budgets.
		// Per-skill override takes precedence over the global cap.
		maxCtx := MaxContextChars
		if skill.MaxContextCharsOverride > 0 && skill.MaxContextCharsOverride < maxCtx {
			maxCtx = skill.MaxContextCharsOverride
		}
		if len(req.Selection) > MaxSelectionChars {
			req.Selection = req.Selection[:MaxSelectionChars]
		}
		if len(req.Context) > maxCtx {
			req.Context = req.Context[:maxCtx]
		}

		// Sanitize language: truncate and strip control characters to
		// prevent prompt injection via the language field.
		req.Language = sanitizeLanguage(req.Language)

		prompt := skill.BuildPrompt(req)

		// Log skill invocation for quality monitoring. Includes
		// estimated token counts so operators can see whether the
		// context budget is being used effectively.
		startTime := time.Now()
		slog.Info("editor skill invoked",
			"skill", string(skillID),
			"model", s.llm.Model(),
			"selection_tokens", EstimateTokens(req.Selection),
			"context_tokens", EstimateTokens(req.Context),
			"prompt_tokens", EstimateTokens(prompt),
		)

		tokenCh, errCh := s.llm.GenerateStream(ctx, prompt)

		tokenCount := 0
		for {
			select {
			case token, ok := <-tokenCh:
				if !ok {
					// Token channel closed — check for errors.
					if err, ok := <-errCh; ok && err != nil {
						slog.Info("editor skill failed",
							"skill", string(skillID),
							"model", s.llm.Model(),
							"elapsed_ms", time.Since(startTime).Milliseconds(),
							"error", err.Error(),
						)
						errs <- err
					} else {
						slog.Info("editor skill completed",
							"skill", string(skillID),
							"model", s.llm.Model(),
							"elapsed_ms", time.Since(startTime).Milliseconds(),
							"output_tokens", tokenCount,
						)
					}
					return
				}
				tokenCount++
				select {
				case tokens <- token:
				case <-ctx.Done():
					return
				}
			case err, ok := <-errCh:
				if !ok {
					// Error channel closed without an error —
					// keep draining tokenCh; it may still
					// have buffered tokens.
					continue
				}
				if err != nil {
					slog.Info("editor skill failed",
						"skill", string(skillID),
						"model", s.llm.Model(),
						"elapsed_ms", time.Since(startTime).Milliseconds(),
						"error", err.Error(),
					)
					errs <- err
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return tokens, errs
}

// --- Prompt builders ---

// sanitizeLanguage truncates the language field and strips characters
// that could be used for prompt injection (newlines, quotes, colons).
func sanitizeLanguage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > MaxLanguageChars {
		s = s[:MaxLanguageChars]
	}
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '"' || r == ':' || r == ';' ||
			r == '`' || r == '<' || r == '>' || r == '{' || r == '}' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func buildImproveWritingPrompt(req SkillRequest) string {
	var b strings.Builder
	b.WriteString("You are an expert editor. Improve the clarity, grammar, and flow of the following text. ")
	b.WriteString("Keep the original meaning. Return only the improved text, no explanations.\n\n")
	b.WriteString("Text to improve:\n")
	b.WriteString(req.Selection)
	b.WriteString("\n")
	if req.Context != "" {
		b.WriteString("\nSurrounding context (for reference only, do not include in output):\n")
		b.WriteString(req.Context)
		b.WriteString("\n")
	}
	return b.String()
}

func buildSummarizePrompt(req SkillRequest) string {
	var b strings.Builder
	b.WriteString("You are an expert summarizer. Summarize the following text in 2-3 sentences. ")
	b.WriteString("Capture the key points. Return only the summary.\n\n")
	b.WriteString("Text to summarize:\n")
	b.WriteString(req.Selection)
	b.WriteString("\n")
	return b.String()
}

func buildExpandPrompt(req SkillRequest) string {
	var b strings.Builder
	b.WriteString("You are an expert writer. Expand the following text with more detail and examples. ")
	b.WriteString("Keep the same tone and style. Return only the expanded text.\n\n")
	b.WriteString("Text to expand:\n")
	b.WriteString(req.Selection)
	b.WriteString("\n")
	if req.Context != "" {
		b.WriteString("\nSurrounding context:\n")
		b.WriteString(req.Context)
		b.WriteString("\n")
	}
	return b.String()
}

func buildSimplifyPrompt(req SkillRequest) string {
	var b strings.Builder
	b.WriteString("You are an expert at simplifying complex text. Rewrite the following text so it is easy to understand for a general audience. ")
	b.WriteString("Keep the core meaning. Return only the simplified text.\n\n")
	b.WriteString("Text to simplify:\n")
	b.WriteString(req.Selection)
	b.WriteString("\n")
	return b.String()
}

func buildTranslatePrompt(req SkillRequest) string {
	var b strings.Builder
	b.WriteString("You are an expert translator. Translate the following text")
	if req.Language != "" {
		b.WriteString(" into ")
		b.WriteString(req.Language)
	} else {
		b.WriteString(" into English")
	}
	b.WriteString(". Return only the translation.\n\n")
	b.WriteString("Text to translate:\n")
	b.WriteString(req.Selection)
	b.WriteString("\n")
	return b.String()
}

func buildGenerateIdeasPrompt(req SkillRequest) string {
	var b strings.Builder
	b.WriteString("You are a creative assistant. Generate 5 ideas related to the following topic. ")
	b.WriteString("Return each idea on a new line, prefixed with a dash. Return only the ideas.\n\n")
	if req.Selection != "" {
		b.WriteString("Topic:\n")
		b.WriteString(req.Selection)
		b.WriteString("\n")
	} else {
		b.WriteString("Topic: (based on the surrounding context below)\n")
	}
	if req.Context != "" {
		b.WriteString("\nContext:\n")
		b.WriteString(req.Context)
		b.WriteString("\n")
	}
	return b.String()
}
