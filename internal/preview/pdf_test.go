package preview

import (
	"context"
	_ "embed"
	"errors"
	"image"
	"os/exec"
	"testing"
)

// minimalPDF is a hand-built 1-page, 200x200 pt PDF used as the test
// fixture. The bytes are checked into the repo via go:embed so the
// test does not depend on any PDF-generating tool at run time —
// pdftoppm itself is the only external dependency we test against.
//
//go:embed testdata/minimal.pdf
var minimalPDF []byte

func TestIsSupportedMimePDF(t *testing.T) {
	t.Parallel()
	if !IsSupportedMime("application/pdf") {
		t.Fatal("IsSupportedMime should accept application/pdf")
	}
	// Mime parsing must be tolerant to capitalisation / whitespace,
	// matching the behaviour for image types.
	if !IsSupportedMime("  Application/PDF  ") {
		t.Fatal("IsSupportedMime should normalise application/pdf")
	}
}

func TestPDFPreviewGeneration(t *testing.T) {
	// Not parallel: TestPDFPreviewGenerationMissingBinary mutates the
	// package-level pdftoppmBinary var, so we run sequentially to
	// avoid races on it.
	if _, err := exec.LookPath(pdftoppmBinary); err != nil {
		// pdftoppm not installed — assert the documented graceful
		// skip: renderPDFFirstPage should return ErrUnsupportedMime
		// so the worker treats the job as a no-op.
		_, gotErr := renderPDFFirstPage(context.Background(), minimalPDF)
		if !errors.Is(gotErr, ErrUnsupportedMime) {
			t.Fatalf("expected ErrUnsupportedMime when pdftoppm is missing, got %v", gotErr)
		}
		t.Skipf("pdftoppm not available; verified graceful skip path")
		return
	}

	img, err := renderPDFFirstPage(context.Background(), minimalPDF)
	if err != nil {
		t.Fatalf("renderPDFFirstPage: %v", err)
	}
	if img == nil {
		t.Fatal("renderPDFFirstPage returned nil image")
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("rendered image has empty bounds: %v", b)
	}
	// Sanity check the page was rasterised at the requested 150 DPI.
	// The fixture is 200 pt x 200 pt; pdftoppm produces a PNG roughly
	// 200 / 72 * 150 ≈ 416 px on each side. Allow generous slack so
	// we do not pin to a specific poppler version.
	if b.Dx() < 200 || b.Dy() < 200 {
		t.Fatalf("rendered image too small for 150 DPI of 200pt page: %v", b)
	}
}

func TestPDFPreviewGenerationMissingBinary(t *testing.T) {
	// Not parallel: mutates package-level state.
	prev := pdftoppmBinary
	t.Cleanup(func() { pdftoppmBinary = prev })
	pdftoppmBinary = "/nonexistent/zkdrive-pdftoppm-stub"

	_, err := renderPDFFirstPage(context.Background(), minimalPDF)
	if !errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("expected ErrUnsupportedMime when binary missing, got %v", err)
	}
}

func TestPDFPreviewGenerationInvalidPDF(t *testing.T) {
	// Not parallel: shares package-level pdftoppmBinary state.
	if _, err := exec.LookPath(pdftoppmBinary); err != nil {
		t.Skip("pdftoppm not available")
	}
	_, err := renderPDFFirstPage(context.Background(), []byte("not a pdf"))
	if err == nil {
		t.Fatal("expected error for invalid PDF input")
	}
	// Invalid input is a hard render failure — must NOT masquerade as
	// ErrUnsupportedMime, otherwise the worker would silently drop a
	// real corruption signal.
	if errors.Is(err, ErrUnsupportedMime) {
		t.Fatalf("invalid PDF should not return ErrUnsupportedMime, got %v", err)
	}
}

// Compile-time guard that renderPDFFirstPage returns the correct type.
var _ func(context.Context, []byte) (image.Image, error) = renderPDFFirstPage
