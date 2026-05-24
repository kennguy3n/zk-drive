package preview

import (
	"context"
	"strings"
	"testing"
)

func TestRenderText_ProducesNonEmptyImage(t *testing.T) {
	t.Parallel()
	src := []byte(strings.Repeat("package preview\nfunc Hello() string { return \"world\" }\n", 4))
	img, err := renderText(context.Background(), src)
	if err != nil {
		t.Fatalf("renderText: %v", err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("rendered image has empty bounds: %v", b)
	}
}

func TestRenderText_TruncatesLongSource(t *testing.T) {
	t.Parallel()
	// Build a source that is provably larger than textPreviewMaxBytes
	// so we exercise the truncation slice path. Any failure here is
	// likely a regression in the cap constant or the slice math.
	src := []byte(strings.Repeat("x", textPreviewMaxBytes*4))
	img, err := renderText(context.Background(), src)
	if err != nil {
		t.Fatalf("renderText: %v", err)
	}
	if img == nil {
		t.Fatal("renderText returned nil image")
	}
}

func TestRenderText_BinaryGarbageDoesNotPanic(t *testing.T) {
	t.Parallel()
	// 0..255 bytes — exercises the non-printable-rune replacement
	// path in renderTextToImage.
	src := make([]byte, 256)
	for i := range src {
		src[i] = byte(i)
	}
	img, err := renderText(context.Background(), src)
	if err != nil {
		t.Fatalf("renderText: %v", err)
	}
	if img == nil {
		t.Fatal("renderText returned nil image")
	}
}

func TestRenderText_IsRegistered(t *testing.T) {
	t.Parallel()
	// Spot-check: text/plain and application/json must both have a
	// renderer wired by init(). If this fails, init() ordering broke
	// somehow.
	for _, m := range []string{"text/plain", "application/json", "application/javascript"} {
		if !IsSupportedMime(m) {
			t.Errorf("expected %q to be registered for text rendering", m)
		}
	}
}

// TestTruncateRunes guards the rune-aware truncation helper because
// regressing it would silently produce U+FFFD glyphs in every
// preview that handles non-ASCII content (CJK / Arabic / emoji
// subjects, multi-byte source files, etc.). The byte-len-based
// path that we replaced would have failed every multi-byte case
// below.
func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		input  string
		max    int
		suffix string
		want   string
	}{
		{
			name: "ASCII within cap is unchanged",
			input: "hello", max: 10, suffix: "…",
			want: "hello",
		},
		{
			name: "ASCII over cap gets ellipsis",
			input: "abcdefghij", max: 5, suffix: "…",
			want: "abcd…",
		},
		{
			name: "CJK over cap slices on rune boundary",
			input: "日本語テストです", max: 4, suffix: "…",
			want: "日本語…",
		},
		{
			name: "emoji over cap slices on rune boundary",
			input: "🎨🎬🎵🎮🎲🃏", max: 3, suffix: "…",
			want: "🎨🎬…",
		},
		{
			name: "empty suffix returns hard cut",
			input: "日本語テストです", max: 3, suffix: "",
			want: "日本語",
		},
		{
			name: "max < 1 yields suffix only",
			input: "anything", max: 0, suffix: "…",
			want: "…",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := truncateRunes(tc.input, tc.max, tc.suffix)
			if got != tc.want {
				t.Errorf("truncateRunes(%q, %d, %q) = %q; want %q",
					tc.input, tc.max, tc.suffix, got, tc.want)
			}
		})
	}
}

// TestWrapLines_MultiByteRuneBoundary guards the rune-aware wrap
// path. The byte-len-based wrap would split each 3-byte CJK rune
// into a leading 1-byte fragment and a trailing 2-byte fragment,
// neither of which is valid UTF-8 in isolation. We assert that
// every wrapped chunk is independently valid UTF-8.
func TestWrapLines_MultiByteRuneBoundary(t *testing.T) {
	t.Parallel()
	// 10 CJK runes; wrap at 4 should give chunks of 4 / 4 / 2 runes.
	body := "日本語テストデータ用"
	lines := wrapLines(body, 4)
	if len(lines) != 3 {
		t.Fatalf("wrapLines len = %d, want 3 (got: %v)", len(lines), lines)
	}
	wantRunes := []int{4, 4, 2}
	for i, ln := range lines {
		got := len([]rune(ln))
		if got != wantRunes[i] {
			t.Errorf("lines[%d] rune count = %d, want %d (line=%q)", i, got, wantRunes[i], ln)
		}
		if !isValidUTF8(ln) {
			t.Errorf("lines[%d] is not valid UTF-8: %q", i, ln)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
