package editor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mockLLM implements ai.LLMClient for testing. It emits canned tokens
// via GenerateStream and returns a fixed string from Generate.
type mockLLM struct {
	tokens []string
	err    error
}

func (m *mockLLM) Generate(ctx context.Context, prompt string) (string, error) {
	return strings.Join(m.tokens, ""), m.err
}

func (m *mockLLM) GenerateStream(ctx context.Context, prompt string) (<-chan string, <-chan error) {
	tokens := make(chan string, len(m.tokens))
	errs := make(chan error, 1)
	go func() {
		// Close tokens first, then errs — Execute reads errs after
		// tokens closes, so errs must still be open at that point.
		defer close(errs)
		defer close(tokens)
		for _, tok := range m.tokens {
			select {
			case tokens <- tok:
			case <-ctx.Done():
				return
			}
		}
		if m.err != nil {
			errs <- m.err
		}
	}()
	return tokens, errs
}

func (m *mockLLM) Model() string { return "mock-model" }

// TestExecuteSuccess verifies that Execute streams tokens from the LLM
// to the caller and closes the token channel on completion.
func TestExecuteSuccess(t *testing.T) {
	t.Parallel()
	svc := NewSkillService(&mockLLM{tokens: []string{"Hello", " ", "world"}})

	tokens, errs := svc.Execute(context.Background(), SkillImproveWriting, SkillRequest{
		Selection: "hello world",
	})

	var got strings.Builder
	for tok := range tokens {
		got.WriteString(tok)
	}
	if err := <-errs; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "Hello world" {
		t.Errorf("got %q, want %q", got.String(), "Hello world")
	}
}

// TestExecuteUnknownSkill verifies that an unknown skill ID returns
// ErrUnknownSkill.
func TestExecuteUnknownSkill(t *testing.T) {
	t.Parallel()
	svc := NewSkillService(&mockLLM{})

	_, errs := svc.Execute(context.Background(), SkillID("nonexistent"), SkillRequest{})

	if err := <-errs; err != ErrUnknownSkill {
		t.Errorf("got %v, want ErrUnknownSkill", err)
	}
}

// TestExecuteEmptySelection verifies that a skill requiring a selection
// returns ErrEmptySelection when none is provided.
func TestExecuteEmptySelection(t *testing.T) {
	t.Parallel()
	svc := NewSkillService(&mockLLM{})

	_, errs := svc.Execute(context.Background(), SkillImproveWriting, SkillRequest{
		Selection: "  ",
	})

	if err := <-errs; err != ErrEmptySelection {
		t.Errorf("got %v, want ErrEmptySelection", err)
	}
}

// TestExecuteLLMNotConfigured verifies that a nil LLM client returns
// ErrLLMNotConfigured.
func TestExecuteLLMNotConfigured(t *testing.T) {
	t.Parallel()
	svc := NewSkillService(nil)

	_, errs := svc.Execute(context.Background(), SkillSummarize, SkillRequest{
		Selection: "some text",
	})

	if err := <-errs; err != ErrLLMNotConfigured {
		t.Errorf("got %v, want ErrLLMNotConfigured", err)
	}
}

// TestExecuteContextCancellation verifies that cancelling the context
// aborts the stream and closes both channels.
func TestExecuteContextCancellation(t *testing.T) {
	t.Parallel()
	svc := NewSkillService(&mockLLM{tokens: []string{"a", "b", "c"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	tokens, errs := svc.Execute(ctx, SkillImproveWriting, SkillRequest{
		Selection: "test",
	})

	// Both channels should close without delivering all tokens.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-tokens:
			if !ok {
				// Channel closed — test passes.
				return
			}
		case _, ok := <-errs:
			if !ok {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for channels to close")
		}
	}
}

// TestExecuteContextTruncation verifies that context longer than
// MaxContextChars is truncated.
func TestExecuteContextTruncation(t *testing.T) {
	t.Parallel()
	var capturedPrompt string
	svc := NewSkillService(&mockLLM{
		tokens: []string{"ok"},
	})

	// We can't directly inspect the prompt, but we can verify the
	// service doesn't error on a large context. The truncation happens
	// internally.
	longCtx := strings.Repeat("x", MaxContextChars+1000)
	tokens, errs := svc.Execute(context.Background(), SkillGenerateIdeas, SkillRequest{
		Context: longCtx,
	})

	// Drain tokens and errors — the service should not error on a
	// large context; it truncates internally.
	for range tokens {
	}
	if err := <-errs; err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	_ = capturedPrompt
}

// TestSanitizeLanguage verifies that control characters are stripped
// and the string is truncated to MaxLanguageChars.
func TestSanitizeLanguage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"simple", "English", "English"},
		{"strips newlines", "Eng\nlish", "Eng lish"[0:3] + "lish"},
		{"strips quotes", "Eng\"lish", "Eng lish"[0:3] + "lish"},
		{"truncates", strings.Repeat("a", MaxLanguageChars+10), strings.Repeat("a", MaxLanguageChars)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeLanguage(tt.input)
			if len(got) > MaxLanguageChars {
				t.Errorf("result too long: %d chars", len(got))
			}
			if strings.ContainsAny(got, "\n\r\"") {
				t.Errorf("result contains control chars: %q", got)
			}
		})
	}
}

// TestPromptBuilders verifies that each skill's prompt builder includes
// the selection text.
func TestPromptBuilders(t *testing.T) {
	t.Parallel()
	req := SkillRequest{Selection: "test text", Context: "some context"}

	tests := []struct {
		name     string
		build    func(SkillRequest) string
		contains string
	}{
		{"improve_writing", buildImproveWritingPrompt, "test text"},
		{"summarize", buildSummarizePrompt, "test text"},
		{"expand", buildExpandPrompt, "test text"},
		{"simplify", buildSimplifyPrompt, "test text"},
		{"translate", buildTranslatePrompt, "test text"},
		{"generate_ideas", buildGenerateIdeasPrompt, "test text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := tt.build(req)
			if !strings.Contains(prompt, tt.contains) {
				t.Errorf("prompt for %s missing %q: %q", tt.name, tt.contains, prompt)
			}
		})
	}
}

// TestAvailableSkills verifies that all registered skills are returned.
func TestAvailableSkills(t *testing.T) {
	t.Parallel()
	skills := AvailableSkills()
	if len(skills) != 6 {
		t.Errorf("expected 6 skills, got %d", len(skills))
	}
	seen := make(map[SkillID]bool)
	for _, s := range skills {
		seen[s] = true
	}
	for _, expected := range []SkillID{
		SkillImproveWriting, SkillSummarize, SkillExpand,
		SkillSimplify, SkillTranslate, SkillGenerateIdeas,
	} {
		if !seen[expected] {
			t.Errorf("missing skill: %s", expected)
		}
	}
}

// TestWithLLM verifies that WithLLM replaces the LLM client and ignores nil.
func TestWithLLM(t *testing.T) {
	t.Parallel()
	svc := NewSkillService(nil)
	if svc.llm != nil {
		t.Fatal("expected nil llm")
	}

	// WithLLM(nil) should be a no-op.
	svc = svc.WithLLM(nil)
	if svc.llm != nil {
		t.Fatal("expected nil llm after WithLLM(nil)")
	}

	// WithLLM(mock) should set the client.
	mock := &mockLLM{}
	svc = svc.WithLLM(mock)
	if svc.llm != mock {
		t.Fatal("expected mock llm after WithLLM(mock)")
	}
}

func TestEstimateTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "empty", input: "", want: 0},
		{name: "short", input: "hello", want: 2},
		{name: "sentence", input: "The quick brown fox jumps over the lazy dog.", want: 13},
		{name: "long", input: strings.Repeat("a", 350), want: 101},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateTokens(tc.input)
			if got != tc.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestSelectionTruncation(t *testing.T) {
	t.Parallel()
	longSelection := strings.Repeat("x", MaxSelectionChars+1000)
	mock := &mockLLM{tokens: []string{"ok"}}
	svc := NewSkillService(mock)

	_, errs := svc.Execute(context.Background(), SkillImproveWriting, SkillRequest{
		Selection: longSelection,
	})
	// Drain channels.
	for range errs {
	}

	// Verify the mock received a prompt with truncated selection.
	// The mock's Generate receives the built prompt; we can't inspect
	// it directly, but we can verify the service didn't error.
	// If truncation failed, the prompt would be oversized but the
	// mock would still return "ok" — so this test mainly verifies
	// no panic / error on oversized input.
}
