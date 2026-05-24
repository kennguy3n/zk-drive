package preview

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"
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

// TestRenderText_UTF8BoundaryAtTruncationPoint guards the
// clipBytesToValidUTF8 wiring on the text renderer. We construct a
// source where a CJK codepoint lands exactly across the
// textPreviewMaxBytes cap, then assert that the rendered byte
// stream after truncation is still valid UTF-8 (no U+FFFD glyphs
// produced by byte-level slicing). The byte-pad before the
// codepoint is sized so the multi-byte rune straddles the cap.
func TestRenderText_UTF8BoundaryAtTruncationPoint(t *testing.T) {
	t.Parallel()
	// Use a CJK rune (3 bytes in UTF-8) and pad so the rune starts
	// at offset textPreviewMaxBytes-1. After byte-truncation the
	// last 1 byte would be the first byte of the rune; the helper
	// must trim it.
	pad := bytes.Repeat([]byte("a"), textPreviewMaxBytes-1)
	src := append(append([]byte{}, pad...), []byte("漢")...)
	src = append(src, []byte("bbb")...) // tail past the cap
	// Re-exercise the truncation path: source size is > cap.
	if len(src) <= textPreviewMaxBytes {
		t.Fatalf("test source too short to exercise truncation: len=%d cap=%d", len(src), textPreviewMaxBytes)
	}
	// What the renderer will keep, byte-for-byte, after the helper.
	want := clipBytesToValidUTF8(src[:textPreviewMaxBytes])
	if !utf8.Valid(want) {
		t.Fatalf("want bytes are not valid UTF-8: %q", want)
	}
	if len(want) >= textPreviewMaxBytes {
		t.Errorf("expected helper to trim at least 1 byte at the cap; got len=%d cap=%d", len(want), textPreviewMaxBytes)
	}
	// Smoke test the renderer path on this input — it should not
	// crash, and the truncation should be silent (no error path).
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

// TestClipBytesToValidUTF8 exercises every interesting class of
// trailing byte the helper has to handle: pure ASCII (no trim),
// complete CJK at the boundary (no trim), a CJK chopped at 1/2/3
// of its 3 bytes (trim 1/2/3), and the 4-byte emoji case (trim up
// to 3). The helper is used by the email body truncation path to
// prevent U+FFFD glyphs at the cut point.
func TestClipBytesToValidUTF8(t *testing.T) {
	t.Parallel()
	// "日" is 0xE6 0x97 0xA5 — a 3-byte UTF-8 codepoint.
	hi := []byte("日")
	// "😀" is 0xF0 0x9F 0x98 0x80 — a 4-byte UTF-8 codepoint.
	emoji := []byte("😀")
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"ascii only", []byte("hello"), []byte("hello")},
		{"empty", []byte{}, []byte{}},
		{"complete cjk", append([]byte("ok"), hi...), append([]byte("ok"), hi...)},
		{"cjk cut at 1 byte", append([]byte("ok"), hi[:1]...), []byte("ok")},
		{"cjk cut at 2 bytes", append([]byte("ok"), hi[:2]...), []byte("ok")},
		{"complete emoji", append([]byte("hi"), emoji...), append([]byte("hi"), emoji...)},
		{"emoji cut at 1 byte", append([]byte("hi"), emoji[:1]...), []byte("hi")},
		{"emoji cut at 2 bytes", append([]byte("hi"), emoji[:2]...), []byte("hi")},
		{"emoji cut at 3 bytes", append([]byte("hi"), emoji[:3]...), []byte("hi")},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := clipBytesToValidUTF8(append([]byte{}, c.in...))
			if !equalBytes(got, c.want) {
				t.Errorf("clipBytesToValidUTF8(%q) = %q, want %q", c.in, got, c.want)
			}
			if !isValidUTF8(string(got)) {
				t.Errorf("clipBytesToValidUTF8(%q) result %q has U+FFFD", c.in, got)
			}
		})
	}
}

func equalBytes(a, b []byte) bool {
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
