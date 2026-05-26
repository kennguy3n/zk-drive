package preview

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

func TestRenderMarkdown_ProducesNonEmptyImage(t *testing.T) {
	t.Parallel()
	src := []byte("# Title\n\nA short paragraph.\n\n## Subhead\n\n- one\n- two\n")
	img, err := renderMarkdown(context.Background(), src)
	if err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("rendered image has empty bounds: %v", b)
	}
}

// TestWalkMarkdownAST_HeadingBecomesBanner verifies the H1 → title +
// uppercased-heading transform. The first H1 fills the title slot
// for the rasteriser's header banner; H1 / H2 are upper-cased in
// the body so they stand out at the rasterised resolution.
func TestWalkMarkdownAST_HeadingBecomesBanner(t *testing.T) {
	t.Parallel()
	src := []byte("# Project README\n\nIntro text\n\n## Setup\n\nDo this.\n\n### Details\n\nFine print.\n")
	title, body := walkMarkdownASTFrom(src)

	if title != "Project README" {
		t.Errorf("expected title 'Project README', got %q", title)
	}
	if !strings.Contains(body, "# PROJECT README") {
		t.Errorf("expected upper-cased H1 banner in body, got: %q", body)
	}
	if !strings.Contains(body, "## SETUP") {
		t.Errorf("expected upper-cased H2 banner in body, got: %q", body)
	}
	if !strings.Contains(body, "### Details") {
		t.Errorf("expected H3 in original case in body, got: %q", body)
	}
}

// TestWalkMarkdownAST_ListBulletsAndNumbering pins the structural
// preservation: bullet vs numbered prefixes, nesting indentation,
// and per-item content extraction. Without this the preview would
// regress to flat paragraphs that lose the list structure entirely.
func TestWalkMarkdownAST_ListBulletsAndNumbering(t *testing.T) {
	t.Parallel()
	src := []byte("- apple\n- banana\n  - banana cavendish\n  - banana plantain\n- cherry\n\n1. first\n2. second\n3. third\n")
	_, body := walkMarkdownASTFrom(src)

	mustContain := []string{
		"- apple",
		"- banana",
		"  - banana cavendish",
		"  - banana plantain",
		"- cherry",
		"1. first",
		"2. second",
		"3. third",
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("expected list line %q in body, got: %q", want, body)
		}
	}
}

// TestWalkMarkdownAST_CodeBlocksFencedAndIndented checks that both
// fenced and 4-space-indented code blocks emit framing sentinels and
// preserve the body verbatim. We DON'T want code blocks to flatten
// into paragraph text — they're a major signal of "this README is a
// dev doc" that we want to convey in the thumbnail.
func TestWalkMarkdownAST_CodeBlocksFencedAndIndented(t *testing.T) {
	t.Parallel()
	src := []byte("Body text.\n\n```go\nfunc main() {}\n```\n\nMore body.\n\n    indented := true\n    other := false\n")
	_, body := walkMarkdownASTFrom(src)

	if !strings.Contains(body, "func main() {}") {
		t.Errorf("fenced code body lost: %q", body)
	}
	if !strings.Contains(body, "indented := true") {
		t.Errorf("indented code body lost: %q", body)
	}
	if !strings.Contains(body, "```") {
		t.Errorf("expected code fence sentinel in output: %q", body)
	}
}

// TestWalkMarkdownAST_EmphasisFlattenedToText verifies emphasis /
// strong / strikethrough markers don't survive into the rasterised
// output — at 256 px monochrome the markers are visual clutter
// without conveying styling. We want the underlying text content
// to remain searchable in the preview.
func TestWalkMarkdownAST_EmphasisFlattenedToText(t *testing.T) {
	t.Parallel()
	src := []byte("This is *italic* and **bold** and `code` text.\n")
	_, body := walkMarkdownASTFrom(src)

	if !strings.Contains(body, "italic") {
		t.Errorf("emphasis content dropped: %q", body)
	}
	if !strings.Contains(body, "bold") {
		t.Errorf("strong content dropped: %q", body)
	}
	if !strings.Contains(body, "code") {
		t.Errorf("code-span content dropped: %q", body)
	}
	// markers should not leak through
	if strings.Contains(body, "*italic*") || strings.Contains(body, "**bold**") || strings.Contains(body, "`code`") {
		t.Errorf("emphasis markers leaked into rasterised body: %q", body)
	}
}

// TestWalkMarkdownAST_BlockquotePrefixCompound pins the > prefix
// emission on every line inside a blockquote, including nested
// children. Without this a blockquote would render as plain text
// and visually merge with surrounding paragraphs in the thumbnail.
func TestWalkMarkdownAST_BlockquotePrefixCompound(t *testing.T) {
	t.Parallel()
	src := []byte("> First quoted line\n> Second quoted line\n\nNormal text.\n")
	_, body := walkMarkdownASTFrom(src)

	if !strings.Contains(body, "> First quoted line Second quoted line") &&
		!strings.Contains(body, "> First quoted line\n> Second quoted line") {
		// goldmark concatenates soft-line-break siblings into one
		// paragraph child; either of the above forms is acceptable.
		t.Errorf("expected blockquote prefix in body, got: %q", body)
	}
}

// TestRenderMarkdown_LongSourceTruncatedSafely asserts the byte-cap
// truncation kicks in for over-large markdown without panicking.
// We don't assert the visible output here because the rasteriser is
// downstream of the parse step — the test exists to prevent a
// regression of the markdownPreviewMaxBytes constant or the slice
// math from re-introducing a slow path on large README dumps.
func TestRenderMarkdown_LongSourceTruncatedSafely(t *testing.T) {
	t.Parallel()
	src := []byte(strings.Repeat("# heading\n\nbody text\n\n", markdownPreviewMaxBytes))
	img, err := renderMarkdown(context.Background(), src)
	if err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	if img == nil {
		t.Fatal("nil image returned")
	}
}

func TestRenderMarkdown_IsRegistered(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"text/markdown", "text/x-markdown"} {
		if !IsSupportedMime(m) {
			t.Errorf("expected %q to be registered for markdown rendering", m)
		}
	}
}

// TestWalkMarkdownAST_GFMTableEmitsTabSeparatedRows pins the GFM
// table extension wiring. Without extension.Table enabled in the
// goldmark constructor, the parser would never produce
// *extast.Table nodes and the emitTable code path would be dead.
// This regression test fails loudly if a future change removes the
// extension or breaks the walker case.
func TestWalkMarkdownAST_GFMTableEmitsTabSeparatedRows(t *testing.T) {
	t.Parallel()
	src := []byte(`# Doc

| Column A | Column B | Column C |
|----------|----------|----------|
| cell 1a  | cell 1b  | cell 1c  |
| cell 2a  | cell 2b  | cell 2c  |
`)
	_, body := walkMarkdownASTFrom(src)

	mustContain := []string{
		"Column A\tColumn B\tColumn C",
		"cell 1a\tcell 1b\tcell 1c",
		"cell 2a\tcell 2b\tcell 2c",
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("expected GFM table row %q in body, got: %q", want, body)
		}
	}
}

// TestWalkMarkdownAST_StrikethroughFlattensToText covers the GFM
// strikethrough extension. Marker characters (~~text~~) must not
// leak into the rasterised output; the inner text must survive.
func TestWalkMarkdownAST_StrikethroughFlattensToText(t *testing.T) {
	t.Parallel()
	src := []byte("This is ~~deleted text~~ in a paragraph.\n")
	_, body := walkMarkdownASTFrom(src)

	if !strings.Contains(body, "deleted text") {
		t.Errorf("strikethrough content dropped: %q", body)
	}
	if strings.Contains(body, "~~deleted text~~") {
		t.Errorf("strikethrough markers leaked into body: %q", body)
	}
}

// TestWalkMarkdownAST_TaskListCheckboxes pins the GFM task-list
// rendering. Each list item starting with `- [ ] ` or `- [x] `
// must surface its checkbox state as a `[ ]` / `[x]` prefix in
// the output — without the extension and the *extast.TaskCheckBox
// walker case, the AST replacement would silently drop the marker.
func TestWalkMarkdownAST_TaskListCheckboxes(t *testing.T) {
	t.Parallel()
	src := []byte(`# Tasks

- [ ] todo item one
- [x] done item two
- [ ] todo item three
`)
	_, body := walkMarkdownASTFrom(src)

	// Marker presence per item:
	if !strings.Contains(body, "[ ] todo item one") {
		t.Errorf("unchecked task marker dropped for item one, got: %q", body)
	}
	if !strings.Contains(body, "[x] done item two") {
		t.Errorf("checked task marker dropped for item two, got: %q", body)
	}
	if !strings.Contains(body, "[ ] todo item three") {
		t.Errorf("unchecked task marker dropped for item three, got: %q", body)
	}
}

// TestWalkMarkdownAST_BlockquoteBlankLineHasPrefix pins the symmetry
// fix in emitBlank: blank separators inside a blockquote carry the
// `> ` prefix so multi-paragraph quotes render as a visually
// contiguous block, matching strict markdown rendering rules.
func TestWalkMarkdownAST_BlockquoteBlankLineHasPrefix(t *testing.T) {
	t.Parallel()
	src := []byte("> First quoted paragraph.\n>\n> Second quoted paragraph after blank.\n\nNormal text after quote.\n")
	_, body := walkMarkdownASTFrom(src)

	// Both content lines must carry the prefix.
	if !strings.Contains(body, "> First quoted paragraph.") {
		t.Errorf("missing prefix on first quoted line, got: %q", body)
	}
	if !strings.Contains(body, "> Second quoted paragraph after blank.") {
		t.Errorf("missing prefix on second quoted line, got: %q", body)
	}
	// And the blank separator between them must also carry it.
	if !strings.Contains(body, "> \n> Second quoted paragraph") {
		// Tolerate either single or compounded prefixes (the
		// implementation collapses duplicates).
		if !strings.Contains(body, "> \n> ") {
			t.Errorf("expected '> ' on blank separator inside blockquote, got: %q", body)
		}
	}
}

// TestWalkMarkdownAST_NoLeadingBlankWhenStartsWithHeading pins the
// fix for the leading-blank cosmetic bug: a markdown document whose
// first block-level element is a heading must NOT produce a leading
// `\n` in the rasterised body. The previous implementation called
// `emitBlank()` before `emitLine(banner)` and the empty-buffer guard
// (suffix-of-`\n\n`) didn't trip on the empty buffer, so a blank line
// leaked through to the top of the preview.
func TestWalkMarkdownAST_NoLeadingBlankWhenStartsWithHeading(t *testing.T) {
	t.Parallel()
	src := []byte("# Top Heading\n\nBody paragraph.\n")
	_, body := walkMarkdownASTFrom(src)
	if strings.HasPrefix(body, "\n") {
		t.Errorf("body should not start with a blank line, got: %q", body)
	}
	// And the heading itself should still be the first line.
	if !strings.HasPrefix(body, "# TOP HEADING") {
		t.Errorf("body should start with the heading banner, got: %q", body)
	}
}

// TestWalkMarkdownAST_EmitBlankIsConstantTime pins the algorithmic
// fix for the emitBlank quadratic-buffer-copy issue. Walks a 1000-
// block document and verifies the walk completes in well under a
// second — the previous out.String() per emitBlank was O(n²) which
// scaled badly even at the 256 KiB source cap.
//
// This test is a soft canary: the real production guard is the
// markdownPreviewMaxBytes cap, but we want a regression to fail
// loudly if someone reintroduces the buffer copy.
func TestWalkMarkdownAST_EmitBlankIsConstantTime(t *testing.T) {
	t.Parallel()
	// Build a 1000-paragraph document — each paragraph triggers
	// the surrounding emitBlank calls. With the old quadratic
	// implementation this took several seconds on a workstation;
	// with the O(1) tracking it's a few milliseconds.
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString("Paragraph text ")
		sb.WriteString(strings.Repeat("x", 64))
		sb.WriteString("\n\n")
	}
	start := time.Now()
	_, _ = walkMarkdownASTFrom([]byte(sb.String()))
	elapsed := time.Since(start)
	// Generous upper bound so the test doesn't flake on slow CI
	// runners but still catches a true quadratic regression.
	if elapsed > 500*time.Millisecond {
		t.Errorf("walkMarkdownAST took %v for 1000 paragraphs; suggests O(n^2) regression", elapsed)
	}
}

// TestWalkMarkdownAST_LooseListContinuationIndented pins the fix for
// the continuation-paragraph indent regression: in a loose list, a
// second paragraph belonging to the same item must render under the
// bullet's text column, not flush-left. Previously the first child
// of a ListItem got `indent + marker`, but the second child walked
// to the generic *ast.Paragraph case which emitted with no prefix,
// causing it to render as a sibling of the list rather than a
// continuation of the item.
//
// The source uses CommonMark's loose-list syntax: blank line between
// the two paragraphs of the same list item, with the second paragraph
// indented by ≥2 spaces in the source so goldmark recognises it as a
// child of the list item rather than the start of a new sibling
// block.
func TestWalkMarkdownAST_LooseListContinuationIndented(t *testing.T) {
	t.Parallel()
	src := []byte("- First paragraph of the list item.\n\n  Second paragraph of the SAME list item.\n\n- Next list item.\n")
	_, body := walkMarkdownASTFrom(src)
	// The continuation paragraph must NOT appear flush-left
	// (which would mean it was rendered as a top-level paragraph
	// outside the list). It must carry the 2-space continuation
	// indent that aligns it under the marker text column.
	if !strings.Contains(body, "  Second paragraph of the SAME list item.") {
		t.Errorf("continuation paragraph should be indented under bullet, got: %q", body)
	}
	// And it must NOT appear at the start of a line with no
	// indent (the old buggy output).
	if strings.Contains(body, "\nSecond paragraph of the SAME list item.") {
		t.Errorf("continuation paragraph must not be flush-left, got: %q", body)
	}
}

// TestWalkMarkdownAST_OrderedLooseListContinuationAlignsUnderMarker
// covers the ordered-list variant — the continuation indent must
// match the WIDTH of the marker (`1. ` = 3 cells, `10. ` = 4 cells)
// rather than a fixed 2-space indent. We verify a single-digit case;
// the multi-digit case is exercised by the indent computation itself
// (`strings.Repeat(" ", len(marker))`).
func TestWalkMarkdownAST_OrderedLooseListContinuationAlignsUnderMarker(t *testing.T) {
	t.Parallel()
	src := []byte("1. First paragraph.\n\n   Continuation under item 1.\n\n2. Second item.\n")
	_, body := walkMarkdownASTFrom(src)
	// Marker `1. ` is 3 cells wide, so continuation must be
	// prefixed with 3 spaces.
	if !strings.Contains(body, "   Continuation under item 1.") {
		t.Errorf("continuation paragraph should align under ordered marker (3-space indent), got: %q", body)
	}
}

// walkMarkdownASTFrom is a test helper that runs the AST walker
// against a fresh goldmark parser instance — mirrors the production
// pipeline in renderMarkdown but returns the (title, body) tuple
// instead of rasterising. Mirrors the extension set used in
// renderMarkdown so the test parser produces the same AST shape
// as production.
func walkMarkdownASTFrom(src []byte) (string, string) {
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.Table,
			extension.Strikethrough,
			extension.TaskList,
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	doc := md.Parser().Parse(text.NewReader(src))
	return walkMarkdownAST(doc, src)
}
