package preview

import (
	"context"
	"errors"
	"image"
	"sort"
	"strings"
	"testing"
)

// TestRendererRegistry asserts the basic contract callers depend on:
// Register / lookup / Unregister / SupportedMimes round-trip, MIME
// normalisation is case- and whitespace-insensitive, and the registry
// is safe for concurrent reads (the worker hits this on every job).
func TestRendererRegistry(t *testing.T) {
	t.Run("Register and lookup", func(t *testing.T) {
		const mime = "x-test/fake-renderer-1"
		// Make sure we start from a clean slate even if a previous
		// test left state behind.
		Unregister(mime)
		t.Cleanup(func() { Unregister(mime) })

		stub := RendererFunc(func(_ context.Context, _ []byte) (image.Image, error) {
			return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
		})
		Register(stub, mime)
		got := lookup(mime)
		if got == nil {
			t.Fatalf("expected a renderer for %q, got nil", mime)
		}
		if !IsSupportedMime(mime) {
			t.Fatalf("IsSupportedMime should report %q as supported after Register", mime)
		}
	})

	t.Run("MIME normalisation", func(t *testing.T) {
		const mime = "x-test/case-norm"
		Unregister(mime)
		t.Cleanup(func() { Unregister(mime) })

		Register(RendererFunc(func(_ context.Context, _ []byte) (image.Image, error) {
			return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
		}), mime)
		for _, variant := range []string{
			"X-Test/Case-Norm",
			"  x-test/case-norm  ",
			"X-TEST/CASE-NORM",
		} {
			if !IsSupportedMime(variant) {
				t.Errorf("expected variant %q to match registered MIME %q", variant, mime)
			}
		}
	})

	t.Run("Unregister", func(t *testing.T) {
		const mime = "x-test/unreg"
		Register(RendererFunc(func(_ context.Context, _ []byte) (image.Image, error) {
			return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
		}), mime)
		if !IsSupportedMime(mime) {
			t.Fatalf("register precondition failed for %q", mime)
		}
		Unregister(mime)
		if IsSupportedMime(mime) {
			t.Fatalf("Unregister did not remove %q from the registry", mime)
		}
	})

	t.Run("SupportedMimes returns sorted set", func(t *testing.T) {
		got := SupportedMimes()
		if len(got) == 0 {
			t.Fatal("SupportedMimes is empty — handler init() blocks did not run")
		}
		// Output must be sorted (callers rely on it for deterministic
		// logs / metric labels).
		if !sort.StringsAreSorted(got) {
			t.Errorf("SupportedMimes not sorted: %v", got)
		}
		// At a minimum, the canonical built-in formats should be
		// registered after package init().
		mustHave := []string{
			"image/png",
			"image/jpeg",
			"image/gif",
			"image/webp",
			"application/pdf",
			"text/plain",
			"application/json",
			"image/svg+xml",
			"application/zip",
			"message/rfc822",
		}
		for _, m := range mustHave {
			found := false
			for _, g := range got {
				if g == m {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %q to be registered after init(), got %v", m, got)
			}
		}
	})

	t.Run("Register with nil renderer panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected Register(nil, ...) to panic")
			}
		}()
		Register(nil, "x-test/should-panic")
	})

	t.Run("Register with empty MIME panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected Register(_, \"\") to panic")
			}
		}()
		Register(RendererFunc(func(_ context.Context, _ []byte) (image.Image, error) {
			return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
		}), "  ")
	})
}

// TestErrUnsupportedDependencyMissing asserts the error-chain contract
// handlers rely on when an external binary (LibreOffice, ffmpeg, etc.)
// is missing — the error MUST match both ErrUnsupportedMime (so the
// worker skips the job gracefully) AND
// ErrUnsupportedDependencyMissing (so logs can attribute the skip to
// a missing dependency rather than an unwired format).
func TestErrUnsupportedDependencyMissing(t *testing.T) {
	err := missingBinaryErr("nonexistent-tool")
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected missingBinaryErr to be ErrUnsupportedMime, got %v", err)
	}
	if !errors.Is(err, ErrUnsupportedDependencyMissing) {
		t.Fatalf("expected missingBinaryErr to be ErrUnsupportedDependencyMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent-tool") {
		t.Fatalf("expected error message to name the missing tool, got %q", err.Error())
	}
}
