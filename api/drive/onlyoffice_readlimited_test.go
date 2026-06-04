package drive

import (
	"bytes"
	"strings"
	"testing"
)

// TestReadLimited pins the save-callback read boundary: a body at or
// below the cap is returned intact, while a body that exceeds it errors
// instead of being silently truncated into a corrupt document version.
func TestReadLimited(t *testing.T) {
	t.Parallel()

	const max = 16

	t.Run("under cap returns full body", func(t *testing.T) {
		t.Parallel()
		want := []byte("hello")
		got, err := readLimited(bytes.NewReader(want), max)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("exactly at cap returns full body", func(t *testing.T) {
		t.Parallel()
		want := bytes.Repeat([]byte("a"), max)
		got, err := readLimited(bytes.NewReader(want), max)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(got) != max {
			t.Fatalf("got len %d, want %d", len(got), max)
		}
	})

	t.Run("over cap errors instead of truncating", func(t *testing.T) {
		t.Parallel()
		src := strings.NewReader(strings.Repeat("a", max+1))
		got, err := readLimited(src, max)
		if err == nil {
			t.Fatalf("expected error for oversized body, got %d bytes", len(got))
		}
	})
}
