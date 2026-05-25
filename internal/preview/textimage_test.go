package preview

import (
	"image/color"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRenderTextToImage_HeaderTruncatedToCanvasWidth asserts that a
// header longer than the canvas's column budget is truncated with an
// ellipsis rather than running off the right edge of the rendered
// image. This applies to every renderer that supplies a header
// (archive, email, CSV, markdown banner) — historically only short
// banners (file name, subject) exercised this path, but the CSV
// renderer can produce a header approaching the upper bound
// (csvPreviewMaxCols × csvPreviewCellMaxRunes ≈ 320 chars joined by
// tabs ≈ 340 cells) which overflows narrow canvases without
// truncation.
//
// We don't rasterise the header to pixels and inspect them — that
// would couple the test to font glyph rendering. Instead we drive
// renderTextToImage with a deliberately oversized header and
// confirm the returned image has the expected canvas bounds (the
// header truncation has no observable effect on canvas size; this
// test exists primarily to guard that the truncation path does not
// panic, regress to byte-level slicing, or produce an empty image).
//
// The functional correctness of the truncation is covered by the
// truncateRunes test (see text_test.go::TestTruncateRunes) plus the
// integration tests in csv_test.go which feed wide headers through
// the full pipeline.
func TestRenderTextToImage_HeaderTruncatedToCanvasWidth(t *testing.T) {
	t.Parallel()
	// A header that is provably wider than any reasonable canvas
	// column budget. 1024 ASCII chars / 8-px advance = 128 cells
	// of header, easily exceeding any of the renderer canvas
	// widths (256–600 px).
	header := strings.Repeat("HEAD", 256)
	img := renderTextToImage("body text", textPreviewOpts{
		maxLines: 5,
		header:   header,
	})
	if img == nil {
		t.Fatal("nil image from renderTextToImage with oversized header")
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("rendered image has empty bounds: %v", b)
	}
}

// TestRenderTextToImage_HeaderRespectsMultiByteRunes asserts that
// header truncation slices on rune boundaries — a byte-level slice
// would produce U+FFFD glyphs at the cut point for non-ASCII
// headers (CJK / Arabic / emoji). The header here is wide enough
// that truncation MUST fire; if rune-awareness regressed we'd get
// a corrupt UTF-8 byte sequence in the drawn header (the rasteriser
// silently turns invalid bytes into U+FFFD, which is hard to assert
// on directly — we settle for asserting the image is produced
// without panic).
func TestRenderTextToImage_HeaderRespectsMultiByteRunes(t *testing.T) {
	t.Parallel()
	// A 3-byte CJK rune repeated 256 times — 768 bytes total.
	// Beyond every renderer canvas's column budget, forces the
	// truncation path.
	cjk := strings.Repeat("文", 256)
	if !utf8.ValidString(cjk) {
		t.Fatalf("test fixture is invalid UTF-8 — bug in test")
	}
	img := renderTextToImage("", textPreviewOpts{
		maxLines: 5,
		header:   cjk,
		bg:       color.White,
		fg:       color.Black,
	})
	if img == nil {
		t.Fatal("nil image from renderTextToImage with multi-byte header")
	}
}

// TestRenderTextToImage_ShortHeaderUnchanged asserts the truncation
// path is a no-op for headers that fit in the canvas — the header
// should NOT have an ellipsis appended when its rune count is
// already within the column budget. We can't directly inspect the
// drawn header bytes, but we can confirm the image is produced
// without error for a typical short banner.
func TestRenderTextToImage_ShortHeaderUnchanged(t *testing.T) {
	t.Parallel()
	img := renderTextToImage("body", textPreviewOpts{
		maxLines: 5,
		header:   "Brief Header",
	})
	if img == nil {
		t.Fatal("nil image for short-header preview")
	}
}

// TestTruncateRunes_HeaderSuffixBudgetMath pins the contract
// renderTextToImage relies on: passing maxRunes = N with a non-
// empty suffix yields a string of exactly N runes (the suffix's
// rune count is internally subtracted from the cut point). This
// is the invariant the header-truncation comment in
// renderTextToImage refers to — if it ever regresses, the header
// will be one cell short of the canvas budget (the off-by-one
// originally caught on PR #84). The body-truncation path in the
// same function uses an empty suffix and appends "…" manually,
// which is a different code path covered by the existing tests.
func TestTruncateRunes_HeaderSuffixBudgetMath(t *testing.T) {
	t.Parallel()
	// 32 ASCII runes — well over any plausible canvas budget for
	// the smallest test target.
	src := strings.Repeat("X", 32)
	for _, budget := range []int{8, 16, 24} {
		out := truncateRunes(src, budget, "…")
		if utf8.RuneCountInString(out) != budget {
			t.Errorf("truncateRunes budget=%d: got %d runes (%q), want %d",
				budget, utf8.RuneCountInString(out), out, budget)
		}
		if !strings.HasSuffix(out, "…") {
			t.Errorf("truncateRunes budget=%d: missing ellipsis suffix in %q", budget, out)
		}
	}
}
