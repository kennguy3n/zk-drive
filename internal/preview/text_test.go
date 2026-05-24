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
