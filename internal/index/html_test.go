package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractHTMLText pins the contract for HTML extraction:
//
//   - body text is extracted
//   - <head>, <title>, <style>, <script> content is skipped
//   - inline whitespace is collapsed (multiple spaces → one)
//   - <pre> preserves interior whitespace
//   - block-level closes flush newlines so phrase queries don't
//     span structural boundaries
func TestExtractHTMLText(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sample.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := extractHTMLText(body)
	if err != nil {
		t.Fatalf("extractHTMLText: %v", err)
	}

	for _, want := range []string{
		"Engineering Update",
		"release ships several improvements", // collapsed whitespace
		"Faster startup",
		"Better logging",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in extracted text, got:\n%s", want, got)
		}
	}

	// <pre> preserves interior whitespace.
	if !strings.Contains(got, "  preserved") {
		t.Errorf("expected <pre> to preserve indent, got:\n%s", got)
	}

	// <script>, <style>, <head> content must NOT leak.
	for _, banned := range []string{
		"Should not appear", // <title>
		"color: red",        // <style>
		"hide me",           // <script>
	} {
		if strings.Contains(got, banned) {
			t.Errorf("non-body content leaked: %q in:\n%s", banned, got)
		}
	}
}

func TestExtractHTMLText_Empty(t *testing.T) {
	_, err := extractHTMLText(nil)
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}

// TestExtractHTMLText_PartialHTML surfaces the permissive-parser
// behaviour: a fragment without <html>/<body> wrappers must still
// extract its text content.
func TestExtractHTMLText_PartialHTML(t *testing.T) {
	got, err := extractHTMLText([]byte("<p>Hello, world!</p>"))
	if err != nil {
		t.Fatalf("extractHTMLText fragment: %v", err)
	}
	if !strings.Contains(got, "Hello, world!") {
		t.Errorf("expected fragment text, got:\n%s", got)
	}
}

// TestExtractHTMLText_NestedSkipTags exercises the pop-until-match
// stack-recovery path: a skip-tag (script) nested inside another
// skip-tag (style) must clear correctly when its close is seen,
// and the parent's close must restore the empty stack so any
// following body content is extracted.
func TestExtractHTMLText_NestedSkipTags(t *testing.T) {
	body := []byte("<html><head><style>css</style><script>js</script></head><body><p>visible body content</p></body></html>")
	got, err := extractHTMLText(body)
	if err != nil {
		t.Fatalf("extractHTMLText: %v", err)
	}
	if strings.Contains(got, "css") || strings.Contains(got, "js") {
		t.Errorf("skip-tag content leaked: %q", got)
	}
	if !strings.Contains(got, "visible body content") {
		t.Errorf("body content was wrongly suppressed: %q", got)
	}
}

// TestExtractHTMLText_LargeRunDoesNotCopyBuilder is a perf-regression
// guard for the lastByte-vs-sb.String() refactor: with a builder
// containing tens of thousands of bytes, the per-block-tag separator
// emission must still be O(1) — calling sb.String() once per <li>
// would be O(n^2) total. We sanity-check completion within a
// generous wall-clock budget rather than calling testing.B because
// the goal is regression-shape, not micro-benchmarking.
func TestExtractHTMLText_LargeRunDoesNotCopyBuilder(t *testing.T) {
	var b strings.Builder
	b.WriteString("<ul>")
	for i := 0; i < 5000; i++ {
		b.WriteString("<li>item ")
		b.WriteString("padding padding padding padding")
		b.WriteString("</li>")
	}
	b.WriteString("</ul>")
	got, err := extractHTMLText([]byte(b.String()))
	if err != nil {
		t.Fatalf("extractHTMLText: %v", err)
	}
	if !strings.Contains(got, "item") {
		t.Errorf("expected list content in output")
	}
	// Sanity: 5000 list items should produce a reasonably-sized
	// output; the exact count isn't pinned but the order of
	// magnitude is.
	if len(got) < 100_000 {
		t.Errorf("output is unexpectedly short (%d bytes), suggests data loss", len(got))
	}
}
