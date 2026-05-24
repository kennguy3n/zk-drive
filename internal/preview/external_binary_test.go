package preview

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os/exec"
	"sync"
	"testing"
)

// binaryVarsMu serialises tests that mutate the package-level binary
// path vars (sofficeBinary, ffmpegBinary, audioWaveformBinary,
// rsvgBinary, imagemagickBinary). Without this lock, anyone who
// later adds t.Parallel() to one of these tests — or to an unrelated
// test in this file that ends up touching the same vars — would
// silently race the assignments and produce intermittent flakes.
// Each test that swaps a binary path takes the lock for its full
// duration via withBinarySwap so the cleanup is paired with the
// lock release.
var binaryVarsMu sync.Mutex

// withBinarySwap takes the package-level lock, calls swap to mutate
// one of the binary vars, registers a t.Cleanup that restores the
// original value, and releases the lock when the test finishes.
// Tests should funnel ALL binary-var mutations through this helper
// rather than touching the vars directly.
func withBinarySwap(t *testing.T, swap func()) {
	t.Helper()
	binaryVarsMu.Lock()
	t.Cleanup(binaryVarsMu.Unlock)
	swap()
}

// These tests are sequential because they all mutate package-level
// binary path vars to exercise the graceful-skip path. The
// binaryVarsMu mutex above keeps them serial even if a future
// refactor adds t.Parallel() to one of them.

func TestRenderOfficeDocument_MissingBinaryIsUnsupported(t *testing.T) {
	prev := sofficeBinary
	withBinarySwap(t, func() {
		sofficeBinary = "/nonexistent/zkdrive-soffice-stub"
	})
	t.Cleanup(func() { sofficeBinary = prev })

	_, err := renderOfficeDocument(context.Background(),
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		[]byte("PK\x03\x04stub"))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when soffice missing, got %v", err)
	}
	if !errors.Is(err, ErrUnsupportedDependencyMissing) {
		t.Fatalf("expected ErrUnsupportedDependencyMissing when soffice missing, got %v", err)
	}
}

func TestRenderOfficeDocument_NoExtensionMappingIsUnsupported(t *testing.T) {
	// Pin soffice + pdftoppm to actual existing paths so the binary
	// check passes and we exercise the extension-mapping branch.
	// We only run this when soffice and pdftoppm are present;
	// otherwise the binary-missing path short-circuits first.
	if _, err := exec.LookPath(sofficeBinary); err != nil {
		t.Skip("soffice not installed; covered by other tests")
	}
	if _, err := exec.LookPath(pdftoppmBinary); err != nil {
		t.Skip("pdftoppm not installed; covered by other tests")
	}
	_, err := renderOfficeDocument(context.Background(), "application/x-totally-unknown-office", []byte("..."))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime for unmapped office mime, got %v", err)
	}
}

func TestRenderVideoFrame_MissingBinaryIsUnsupported(t *testing.T) {
	prev := ffmpegBinary
	withBinarySwap(t, func() {
		ffmpegBinary = "/nonexistent/zkdrive-ffmpeg-stub"
	})
	t.Cleanup(func() { ffmpegBinary = prev })

	_, err := renderVideoFrame(context.Background(), []byte("\x00\x00\x00stub"))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when ffmpeg missing, got %v", err)
	}
}

func TestRenderAudioWaveform_NoToolsIsUnsupported(t *testing.T) {
	prevBBC := audioWaveformBinary
	prevFF := ffmpegBinary
	withBinarySwap(t, func() {
		audioWaveformBinary = "/nonexistent/zkdrive-audiowaveform-stub"
		ffmpegBinary = "/nonexistent/zkdrive-ffmpeg-stub"
	})
	t.Cleanup(func() {
		audioWaveformBinary = prevBBC
		ffmpegBinary = prevFF
	})

	_, err := renderAudioWaveform(context.Background(), []byte("\x00stub"))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when no audio tool is installed, got %v", err)
	}
}

func TestRenderSVG_MissingBinaryIsUnsupported(t *testing.T) {
	prev := rsvgBinary
	withBinarySwap(t, func() {
		rsvgBinary = "/nonexistent/zkdrive-rsvg-stub"
	})
	t.Cleanup(func() { rsvgBinary = prev })

	_, err := renderSVG(context.Background(), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when rsvg-convert missing, got %v", err)
	}
}

func TestRenderDesign_MissingBinaryIsUnsupported(t *testing.T) {
	prev := imagemagickBinary
	withBinarySwap(t, func() {
		imagemagickBinary = "/nonexistent/zkdrive-convert-stub"
	})
	t.Cleanup(func() { imagemagickBinary = prev })

	_, err := renderDesign(context.Background(), []byte("not a psd"))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when ImageMagick missing, got %v", err)
	}
}

// TestRenderViaSubprocess_SuccessPath exercises the happy path of the
// shared subprocess helper using a fake "tool" that just copies a
// known good PNG fixture to the requested output path. This validates
// the placeholder substitution + decode chain end-to-end without
// requiring any real external tool.
func TestRenderViaSubprocess_SuccessPath(t *testing.T) {
	// Use /bin/sh + cp as the "tool" — sh is everywhere CI runs.
	// We rewrite the placeholders into a shell command that copies
	// the source bytes to the output path. The image bytes are the
	// 1x1 transparent PNG we have to inline because we don't want to
	// depend on a fixture file.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("cp"); err != nil {
		t.Skip("cp not available")
	}
	// Encode a real 1x1 PNG on the fly. Pinning a hex-coded PNG
	// snapshot in the test source has bitten us before (the inline
	// bytes I tried first were a corrupt copy from somewhere), so
	// generating it through the stdlib encoder is both shorter and
	// guaranteed to round-trip through image.Decode.
	pngBuf := bytes.Buffer{}
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	// fake "convert" — copy input to output. Placeholder
	// substitution gives us absolute paths, so cp Just Works.
	img, err := renderViaSubprocess(context.Background(), "cp", "in.png", "out.png",
		[]string{"{{in}}", "{{out}}"},
		pngBuf.Bytes(),
	)
	if err != nil {
		t.Fatalf("renderViaSubprocess: %v", err)
	}
	if img == nil {
		t.Fatal("renderViaSubprocess returned nil image")
	}
}

// TestRenderViaSubprocess_PlaceholderSubstitutionInTokens guards the
// design.go bug regression: ImageMagick's `{{in}}[0]` syntax embeds
// the placeholder inside a larger argument token and relies on
// strings.ReplaceAll-style substitution. An exact-match
// implementation would pass `{{in}}[0]` through as a literal and
// break every design preview. We exercise this via /bin/sh so the
// test doesn't need ImageMagick installed.
func TestRenderViaSubprocess_PlaceholderSubstitutionInTokens(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	pngBuf := bytes.Buffer{}
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	// sh -c 'cat "$1" > "$2"' -- {{in}}[0] {{out}}
	// The "[0]" suffix is stripped by sh before cat sees the path
	// (we use parameter expansion ${1%[0]} to drop it). This proves
	// that {{in}} is substituted INSIDE "{{in}}[0]" rather than
	// being passed through as the literal placeholder.
	img, err := renderViaSubprocess(context.Background(), "sh", "in.png", "out.png",
		[]string{
			"-c",
			`cat "${1%\[0\]}" > "$2"`,
			"--",
			"{{in}}[0]",
			"{{out}}",
		},
		pngBuf.Bytes(),
	)
	if err != nil {
		t.Fatalf("renderViaSubprocess with embedded placeholder: %v", err)
	}
	if img == nil {
		t.Fatal("renderViaSubprocess returned nil image")
	}
}
