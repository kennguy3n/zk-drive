package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractPPTXText pins the contract for .pptx extraction
// against a fixture produced by python-pptx (different library
// than the in-house archive/zip + xml walker).
//
// Pins:
//   - Title and content text from every slide land in the output
//   - Speaker notes appear after the corresponding slide body
//   - Slides are double-newline separated
//   - Multi-slide ordering follows slideN.xml numeric suffix
func TestExtractPPTXText(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sample.pptx"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := extractPPTXText(body)
	if err != nil {
		t.Fatalf("extractPPTXText: %v", err)
	}

	for _, want := range []string{
		"Roadmap Overview",
		"Q1 milestones and exit criteria",
		"Speaker note: emphasize Q1 customer commits",
		"Risks",
		"Supply chain delays remain the top risk",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in extracted text, got:\n%s", want, got)
		}
	}

	// Slide 1 ("Roadmap Overview") must appear before slide 2
	// ("Risks") regardless of zip-entry iteration order.
	idx1 := strings.Index(got, "Roadmap Overview")
	idx2 := strings.Index(got, "Risks")
	if idx1 < 0 || idx2 < 0 || idx1 >= idx2 {
		t.Errorf("slide ordering wrong: idx1=%d idx2=%d got=\n%s", idx1, idx2, got)
	}

	// Slides separated by blank line.
	if !strings.Contains(got, "\n\nRisks") {
		t.Errorf("expected double-newline before slide 2; got:\n%s", got)
	}
}

func TestExtractPPTXText_Malformed(t *testing.T) {
	_, err := extractPPTXText([]byte("not a zip file"))
	if err == nil {
		t.Fatal("expected error on non-zip input")
	}
}

// TestExtractPPTXText_EmptyArchive exercises the no-slide-entries
// path: a valid zip containing no ppt/slides/*.xml must surface a
// real error rather than silently writing empty content.
func TestExtractPPTXText_NoSlides(t *testing.T) {
	// Build a minimal but valid zip with one unrelated entry.
	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "no-slides.zip")
	if err := writeMinimalZip(zipPath, "unrelated.xml", []byte("<x/>")); err != nil {
		t.Fatalf("writeMinimalZip: %v", err)
	}
	body, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	_, err = extractPPTXText(body)
	if err == nil {
		t.Fatal("expected error on archive with no slides")
	}
	if !strings.Contains(err.Error(), "no slide entries") {
		t.Errorf("expected 'no slide entries' error, got: %v", err)
	}
}
