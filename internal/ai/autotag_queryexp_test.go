package ai

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtensionTagOf(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"pdf lowercase", "report.pdf", "pdf"},
		{"pdf uppercase", "REPORT.PDF", "pdf"},
		{"xlsx", "budget.xlsx", "spreadsheet"},
		{"odt", "memo.odt", "document"},
		{"pptx", "deck.pptx", "presentation"},
		{"md", "notes.md", "markdown"},
		{"unknown extension", "file.weirdext", ""},
		{"no extension", "README", ""},
		{"trailing dot", "name.", ""},
		{"dotfile not in map", ".gitignore", ""}, // unmapped extension -> ""
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extensionTagOf(tc.in)
			if got != tc.want {
				t.Fatalf("extensionTagOf(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalTag(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"  Marketing-Q4  ", "marketing-q4"},
		{"Q4_2024", "q4_2024"},
		{"foo/bar", ""},     // reject path-separator
		{"100%off", ""},     // reject %-encoded sentinel
		{"   ", ""},         // empty after trim
		{"", ""},            // already empty
		{strings.Repeat("a", 65), ""}, // exceeds MaxTagLength=64
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
		// 22 CJK characters = 66 bytes (3 bytes per char) — must
		// be rejected because file.Service.AddTag would reject
		// 66 > 64 even though the rune count is 22 ≤ 64. The
		// rune-vs-byte mismatch on this very line was the bug
		// caught by Devin Review on PR #85.
		{strings.Repeat("文", 22), ""},
		// 21 CJK characters = 63 bytes — fits within 64-byte
		// budget so should pass. Note: we lowercase via
		// strings.ToLower which is a no-op for these CJK code
		// points, so the canonical form is identical to input.
		{strings.Repeat("文", 21), strings.Repeat("文", 21)},
		// Mixed multi-byte: 32 chars × 2 bytes ("é" = U+00E9 in
		// UTF-8 is 0xC3 0xA9, 2 bytes) = 64 bytes — fits exactly.
		{strings.Repeat("é", 32), strings.Repeat("é", 32)},
		// 33 chars × 2 bytes = 66 bytes — must be rejected.
		{strings.Repeat("é", 33), ""},
	}
	for _, tc := range cases {
		got := canonicalTag(tc.in)
		if got != tc.want {
			t.Fatalf("canonicalTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseTagLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"clean newlines", "alpha\nbeta\ngamma", []string{"alpha", "beta", "gamma"}},
		{"with hash + bullets", "# alpha\n* beta\n- gamma\n", []string{"alpha", "beta", "gamma"}},
		{"comma per line", "alpha, beta\ngamma", []string{"alpha", "beta", "gamma"}},
		{"dedupe", "alpha\nALPHA\nalpha", []string{"alpha"}},
		{"reject invalid", "alpha\nfoo/bar\nbeta", []string{"alpha", "beta"}},
		{"quoted", "\"alpha\"\n'beta'", []string{"alpha", "beta"}},
		{"empty lines + trailing comma", "alpha,\n\nbeta,", []string{"alpha", "beta"}},
		// Devin Review ANALYSIS_0003: a model that emits
		// "alpha,#beta" on one line — the previous parseTagLines
		// implementation trimmed only at line edges, so the "#"
		// in front of "beta" leaked through to canonicalTag
		// (which doesn't reject "#"), producing the literal
		// "#beta" as a suggestion. The per-part trim added in
		// this commit strips a single leading "#"/quote rune off
		// each comma-separated piece.
		{"hash and star on interior comma parts", "alpha,#beta,*gamma", []string{"alpha", "beta", "gamma"}},
		{"hash and quote on interior parts", "alpha, #beta, \"gamma\"", []string{"alpha", "beta", "gamma"}},
		// Tags that legitimately start with a hyphen used to be
		// corrupted (".-foo-bar" became "foo-bar") because the
		// edge trim stripped leading hyphens. Now only "- " (a
		// space-terminated bullet) is recognised as a list
		// marker, so a hyphen-prefixed tag survives.
		{"hyphen-prefixed tag", "-foo-bar", []string{"-foo-bar"}},
		// And the standard "- tag" bullet still gets stripped.
		{"bullet tag", "- foo-bar", []string{"foo-bar"}},
		// Numbered-list bullet should be stripped without
		// touching internal hyphens.
		{"numbered list", "1. q4-launch\n2. marketing-2024", []string{"q4-launch", "marketing-2024"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTagLines(tc.in)
			if !slicesEqual(got, tc.want) {
				t.Fatalf("parseTagLines(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractKeywords(t *testing.T) {
	text := "The marketing team reviewed the Q4 marketing report. Marketing is critical."
	got := extractKeywords(text, 5)
	// Should contain "marketing" (highest freq, len >= 4).
	found := false
	for _, k := range got {
		if k == "marketing" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("extractKeywords(%q) = %v, expected to contain 'marketing'", text, got)
	}
	// Should reject "the" (len < 4 floor).
	for _, k := range got {
		if k == "the" {
			t.Fatalf("extractKeywords returned short stopword 'the'")
		}
	}
	// Should reject "q4" via all-digit-rejection NOT applying (q4 has letter) — but len(q4)=2 < 4, so rejected.
	for _, k := range got {
		if k == "q4" {
			t.Fatalf("extractKeywords returned too-short 'q4'")
		}
	}
}

func TestExtractKeywordsRejectsAllDigits(t *testing.T) {
	text := "12345 67890 testing reporting customer"
	got := extractKeywords(text, 5)
	for _, k := range got {
		if k == "12345" || k == "67890" {
			t.Fatalf("extractKeywords returned all-digit token %q", k)
		}
	}
}

func TestRuleBasedSuggestions(t *testing.T) {
	suggestions := ruleBasedSuggestions(
		"q4-launch-plan.pdf",
		"Marketing launch plan for Q4 2024. Customer outreach strategy.",
		[]string{"marketing-q4-2024", "internal", "draft"},
	)
	if len(suggestions) == 0 {
		t.Fatal("ruleBasedSuggestions returned empty list")
	}
	// First must be the extension tag (pdf).
	if suggestions[0] != "pdf" {
		t.Fatalf("ruleBasedSuggestions[0] = %q, want %q", suggestions[0], "pdf")
	}
	// Workspace overlap: "marketing-q4-2024" should appear because
	// the body mentions "Marketing" and "Q4 2024".
	hasOverlap := false
	for _, s := range suggestions {
		if s == "marketing-q4-2024" {
			hasOverlap = true
		}
	}
	if !hasOverlap {
		t.Fatalf("expected marketing-q4-2024 in suggestions, got %v", suggestions)
	}
}

func TestMergeSuggestions(t *testing.T) {
	ruleBased := []string{"pdf", "marketing", "launch"}
	llm := []string{"marketing", "outreach", "strategy"}
	got := mergeSuggestions(ruleBased, llm)
	want := []string{"pdf", "marketing", "launch", "outreach", "strategy"}
	if !slicesEqual(got, want) {
		t.Fatalf("mergeSuggestions = %v, want %v", got, want)
	}
}

func TestBuildTagSuggestPromptLocalisesInstruction(t *testing.T) {
	french := BuildTagSuggestPrompt("notes.md", "Sample content", []string{"draft"}, "french")
	english := BuildTagSuggestPrompt("notes.md", "Sample content", []string{"draft"}, "english")
	if !strings.Contains(french, "français") {
		t.Fatalf("french prompt should contain 'français', got:\n%s", french)
	}
	if strings.Contains(french, "Answer in English.") {
		t.Fatalf("french prompt should NOT contain 'Answer in English.', got:\n%s", french)
	}
	if !strings.Contains(english, "Answer in English.") {
		t.Fatalf("english prompt should contain 'Answer in English.', got:\n%s", english)
	}
	// Both must contain the user-content half verbatim.
	for _, p := range []string{french, english} {
		if !strings.Contains(p, "notes.md") {
			t.Fatalf("prompt missing filename: %s", p)
		}
		if !strings.Contains(p, "Sample content") {
			t.Fatalf("prompt missing content preview: %s", p)
		}
	}
}

func TestExtractExpansionTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"basic", "marketing launch plan", []string{"marketing", "launch", "plan"}},
		{"deduped", "report report 2024", []string{"report", "2024"}},
		{"single char dropped", "a x marketing", []string{"marketing"}}, // single-char tokens dropped
		{"two char kept", "ai bi marketing", []string{"ai", "bi", "marketing"}},
		{"mixed", "q4 launch marketing 2024", []string{"q4", "launch", "marketing", "2024"}},
		{"punctuation split", "user-launch,plan", []string{"user", "launch", "plan"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractExpansionTokens(tc.in)
			if !slicesEqual(got, tc.want) {
				t.Fatalf("extractExpansionTokens(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestContainsHyphenSegment(t *testing.T) {
	cases := []struct {
		s, tok string
		want   bool
	}{
		{"q4-2024", "q4", true},
		{"q4-2024", "2024", true},
		{"iq40-survey", "q4", false}, // not hyphen-bounded
		{"marketing-q4-2024", "q4", true},
		{"marketing", "marketing", true},
		{"marketing-launch", "launch", true},
		{"marketing-launch", "market", false}, // not segment; "market" is prefix without hyphen
	}
	for _, tc := range cases {
		got := containsHyphenSegment(tc.s, tc.tok)
		if got != tc.want {
			t.Fatalf("containsHyphenSegment(%q,%q) = %v, want %v", tc.s, tc.tok, got, tc.want)
		}
	}
}

func TestRuleBasedExpansion(t *testing.T) {
	tags := []string{"marketing-q4-2024", "marketing-q3-2024", "internal", "customer-feedback", "draft"}

	t.Run("hyphen segment beats substring", func(t *testing.T) {
		got := ruleBasedExpansion("q4 launch", tags)
		// "marketing-q4-2024" matches "q4" as a hyphen-bounded
		// segment (+2) so it should be ranked highest. The other
		// tags either don't match or match weakly.
		if len(got) == 0 || got[0] != "marketing-q4-2024" {
			t.Fatalf("expected marketing-q4-2024 first, got %v", got)
		}
	})

	t.Run("substring matches", func(t *testing.T) {
		got := ruleBasedExpansion("market", tags)
		// Both marketing tags should match by substring.
		hasQ4 := false
		hasQ3 := false
		for _, t := range got {
			if t == "marketing-q4-2024" {
				hasQ4 = true
			}
			if t == "marketing-q3-2024" {
				hasQ3 = true
			}
		}
		if !hasQ4 || !hasQ3 {
			t.Fatalf("expected both marketing tags in expansion, got %v", got)
		}
	})

	t.Run("no match returns empty", func(t *testing.T) {
		got := ruleBasedExpansion("completely-unrelated", tags)
		if len(got) != 0 {
			t.Fatalf("expected empty expansion, got %v", got)
		}
	})

	t.Run("empty workspace tags", func(t *testing.T) {
		got := ruleBasedExpansion("anything", nil)
		if len(got) != 0 {
			t.Fatalf("expected empty expansion for no workspace tags, got %v", got)
		}
	})
}

func TestBuildQueryExpansionPromptLocalises(t *testing.T) {
	german := BuildQueryExpansionPrompt("marketing launch", []string{"q4"}, "german")
	english := BuildQueryExpansionPrompt("marketing launch", []string{"q4"}, "english")
	if !strings.Contains(german, "Deutsch") {
		t.Fatalf("german prompt should contain 'Deutsch', got:\n%s", german)
	}
	if !strings.Contains(english, "Answer in English.") {
		t.Fatalf("english prompt should contain 'Answer in English.', got:\n%s", english)
	}
	// User content half is identical across languages.
	for _, p := range []string{german, english} {
		if !strings.Contains(p, "marketing launch") {
			t.Fatalf("prompt missing query: %s", p)
		}
	}
}

func TestTruncatePreviewCutsOnRuneBoundary(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"empty input", "", 32, ""},
		{"under budget", "hello", 32, "hello"},
		{"exact budget", "hello", 5, "hello"},
		{"ascii over budget", "hello world", 5, "hello"},
		{"negative budget", "hello", -1, ""},
		{"zero budget", "hello", 0, ""},
		// "文" is 3 bytes in UTF-8 (U+6587 → 0xE6 0x96 0x87).
		// Naive cut at byte 4 would split the second rune.
		// truncatePreview must back up to byte 3 so the prefix is
		// a complete rune.
		{"cut mid-CJK rune", "文文文", 4, "文"},
		{"cut at CJK boundary", "文文文", 3, "文"},
		{"cut at CJK boundary 6", "文文文", 6, "文文"},
		// "café" is c(1) a(1) f(1) é(2 bytes: 0xC3 0xA9) = 5 bytes.
		// maxBytes=3 → byte 3 is 0xC3 (rune start). No backup.
		// Result = "caf" (3 bytes).
		{"cut at 3byte boundary in 'café'", "café", 3, "caf"},
		// maxBytes=4 → byte 4 is 0xA9 (continuation byte).
		// Must back up to byte 3 (the 0xC3 rune-start) → "caf".
		{"cut mid-é continuation byte", "café", 4, "caf"},
		// maxBytes=5 → exact full-string length, no truncation.
		{"cut at full length", "café", 5, "café"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncatePreview(tc.in, tc.maxBytes)
			if got != tc.want {
				t.Fatalf("truncatePreview(%q, %d) = %q (%d bytes), want %q (%d bytes)",
					tc.in, tc.maxBytes, got, len(got), tc.want, len(tc.want))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncatePreview returned invalid UTF-8 for input %q, maxBytes=%d: %q", tc.in, tc.maxBytes, got)
			}
		})
	}
}

func TestCorpusTokenSetSplitsOnNonAlnum(t *testing.T) {
	got := corpusTokenSet("hello, world! q4-2024 plan")
	for _, want := range []string{"hello", "world", "q4", "2024", "plan"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("corpusTokenSet missing %q in %v", want, keysOf(got))
		}
	}
	// Hyphenated forms should NOT appear as a single token — the
	// hyphen splits them.
	if _, ok := got["q4-2024"]; ok {
		t.Fatalf("corpusTokenSet contained hyphenated token (should be split): %v", keysOf(got))
	}
}

func TestRuleBasedSuggestionsShortPartUsesWordBoundary(t *testing.T) {
	// Tag "ai-fast" splits into ["ai", "fast"]. Naive substring
	// matching would say "ai" is present in "main", "explain",
	// "again", etc. — false positive. Word-boundary matching
	// requires the corpus to contain "ai" as a standalone token.
	//
	// Negative case: body text contains "main" and "rain" but no
	// standalone "ai" → ai-fast must NOT be suggested.
	suggestionsNeg := ruleBasedSuggestions(
		"general-notes.pdf",
		"The main highway during the rain was again congested.",
		[]string{"ai-fast"},
	)
	for _, s := range suggestionsNeg {
		if s == "ai-fast" {
			t.Fatalf("ai-fast should NOT match body without standalone 'ai' token; got suggestions %v", suggestionsNeg)
		}
	}

	// Positive case: body contains "ai" as a standalone word →
	// ai-fast suggested as expected.
	suggestionsPos := ruleBasedSuggestions(
		"ai-roadmap.pdf",
		"The team's AI strategy this quarter is to ship fast.",
		[]string{"ai-fast"},
	)
	found := false
	for _, s := range suggestionsPos {
		if s == "ai-fast" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ai-fast should match body with standalone 'ai' and 'fast' tokens; got suggestions %v", suggestionsPos)
	}

	// Also verify "x-ray" doesn't trigger on every document
	// containing the letter "x" — same false-positive class.
	suggestionsX := ruleBasedSuggestions(
		"text-export.pdf",
		"This export contains example text, next steps, and extra context.",
		[]string{"x-ray"},
	)
	for _, s := range suggestionsX {
		if s == "x-ray" {
			t.Fatalf("x-ray should NOT match body containing only 'x' substrings; got %v", suggestionsX)
		}
	}
}

func TestRuleBasedSuggestionsRejectsAllEmptyPartsTag(t *testing.T) {
	// Devin Review ANALYSIS_0005: a workspace tag composed entirely
	// of separator runs (e.g. "---" or "-") splits into all-empty
	// parts. The empty-part skip means the inner loop never fails
	// allPresent, so before this fix the tag was suggested for
	// every file. matchedAnyPart now gates the append on at least
	// one non-empty part having actually matched the corpus.
	for _, tag := range []string{"---", "-", "--", "----"} {
		got := ruleBasedSuggestions(
			"some-file.pdf",
			"this is some sample body text",
			[]string{tag},
		)
		for _, s := range got {
			if s == tag {
				t.Fatalf("ruleBasedSuggestions should NOT suggest all-separator tag %q for body without it; got %v", tag, got)
			}
		}
	}

	// Sanity check: a well-formed multi-part tag still matches
	// when both parts are present as standalone tokens.
	got := ruleBasedSuggestions(
		"q4-launch.pdf",
		"the q4 launch is on track and the report is in good shape",
		[]string{"q4-launch"},
	)
	found := false
	for _, s := range got {
		if s == "q4-launch" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ruleBasedSuggestions should still suggest well-formed multi-part tag q4-launch; got %v", got)
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
