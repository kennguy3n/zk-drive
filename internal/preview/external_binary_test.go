package preview

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os/exec"
	"testing"
)

// These tests are sequential because they all mutate package-level
// binary path vars to exercise the graceful-skip path. Running them
// in parallel would race the var assignments.

func TestRenderOfficeDocument_MissingBinaryIsUnsupported(t *testing.T) {
	prev := sofficeBinary
	t.Cleanup(func() { sofficeBinary = prev })
	sofficeBinary = "/nonexistent/zkdrive-soffice-stub"

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
	t.Cleanup(func() { ffmpegBinary = prev })
	ffmpegBinary = "/nonexistent/zkdrive-ffmpeg-stub"

	_, err := renderVideoFrame(context.Background(), []byte("\x00\x00\x00stub"))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when ffmpeg missing, got %v", err)
	}
}

func TestRenderAudioWaveform_NoToolsIsUnsupported(t *testing.T) {
	prevBBC := audioWaveformBinary
	prevFF := ffmpegBinary
	t.Cleanup(func() {
		audioWaveformBinary = prevBBC
		ffmpegBinary = prevFF
	})
	audioWaveformBinary = "/nonexistent/zkdrive-audiowaveform-stub"
	ffmpegBinary = "/nonexistent/zkdrive-ffmpeg-stub"

	_, err := renderAudioWaveform(context.Background(), []byte("\x00stub"))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when no audio tool is installed, got %v", err)
	}
}

func TestRenderSVG_MissingBinaryIsUnsupported(t *testing.T) {
	prev := rsvgBinary
	t.Cleanup(func() { rsvgBinary = prev })
	rsvgBinary = "/nonexistent/zkdrive-rsvg-stub"

	_, err := renderSVG(context.Background(), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when rsvg-convert missing, got %v", err)
	}
}

func TestRenderDesign_MissingBinaryIsUnsupported(t *testing.T) {
	prev := imagemagickBinary
	t.Cleanup(func() { imagemagickBinary = prev })
	imagemagickBinary = "/nonexistent/zkdrive-convert-stub"

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
