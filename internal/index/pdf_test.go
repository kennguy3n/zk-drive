package index

import (
	"context"
	_ "embed"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// minimalPDF is a hand-built 1-page, 200x200 pt PDF used as the test
// fixture. The bytes are checked into the repo via go:embed so the
// test does not depend on any PDF-generating tool at run time —
// pdftotext itself is the only external dependency we test against.
//
//go:embed testdata/minimal.pdf
var minimalPDF []byte

func TestExtractPDFText(t *testing.T) {
	// Not parallel: TestExtractPDFTextMissingBinary mutates the
	// package-level pdftotextBinary var, so we run sequentially to
	// avoid races on it.
	if _, err := exec.LookPath(pdftotextBinary); err != nil {
		// pdftotext not installed — assert the documented graceful
		// skip: extractPDFText should return ErrUnsupportedMimeType
		// so the worker treats the job as a no-op.
		_, gotErr := extractPDFText(context.Background(), minimalPDF)
		if !errors.Is(gotErr, ErrUnsupportedMimeType) {
			t.Fatalf("expected ErrUnsupportedMimeType when pdftotext is missing, got %v", gotErr)
		}
		t.Skipf("pdftotext not available; verified graceful skip path")
		return
	}

	text, err := extractPDFText(context.Background(), minimalPDF)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	// The fixture has the literal string "Hello PDF" embedded in its
	// content stream. We don't pin the exact whitespace/newlines
	// pdftotext emits (varies by poppler version), just assert the
	// canonical body word survived.
	if !strings.Contains(text, "Hello PDF") {
		t.Fatalf("expected extracted text to contain %q; got %q", "Hello PDF", text)
	}
}

func TestExtractPDFTextMissingBinary(t *testing.T) {
	// Not parallel: mutates package-level state.
	prev := pdftotextBinary
	t.Cleanup(func() { pdftotextBinary = prev })
	pdftotextBinary = "/nonexistent/zkdrive-pdftotext-stub"

	_, err := extractPDFText(context.Background(), minimalPDF)
	if !errors.Is(err, ErrUnsupportedMimeType) {
		t.Fatalf("expected ErrUnsupportedMimeType when binary missing, got %v", err)
	}
}

func TestExtractPDFTextInvalidPDF(t *testing.T) {
	// Not parallel: shares package-level pdftotextBinary state.
	if _, err := exec.LookPath(pdftotextBinary); err != nil {
		t.Skip("pdftotext not available")
	}
	_, err := extractPDFText(context.Background(), []byte("not a pdf"))
	if err == nil {
		t.Fatal("expected error for invalid PDF input")
	}
	// Invalid input is a hard extract failure — must NOT masquerade
	// as ErrUnsupportedMimeType, otherwise the worker would silently
	// drop a real corruption signal. Same correctness invariant
	// internal/preview/pdf_test.go pins for the preview path.
	if errors.Is(err, ErrUnsupportedMimeType) {
		t.Fatalf("invalid PDF should not return ErrUnsupportedMimeType, got %v", err)
	}
}

func TestExtractTextRoutesPDFThroughPDFExtractor(t *testing.T) {
	if _, err := exec.LookPath(pdftotextBinary); err != nil {
		t.Skip("pdftotext not available")
	}
	// ExtractText should route application/pdf through extractPDFText
	// and then apply the same UTF-8 truncate pass as the text/* path.
	got, err := ExtractText("application/pdf", minimalPDF)
	if err != nil {
		t.Fatalf("ExtractText(application/pdf): %v", err)
	}
	if !strings.Contains(got, "Hello PDF") {
		t.Fatalf("ExtractText routed PDF but body word missing; got %q", got)
	}
}
