package index

import (
	"archive/zip"
	"bytes"
	"fmt"
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

// TestExtractPPTXText_SparseNotesPairedBySuffix pins the
// architectural regression: when speaker notes are SPARSE
// (e.g. slide 1 + slide 2 + slide 3 but only notesSlide1 +
// notesSlide3 — no notesSlide2), the extractor must pair each
// note to its slide by numeric suffix, not by positional index.
// The pre-fix implementation iterated `notes[i]` which would have
// attached notesSlide3 to slide 2 and left slide 3 noteless.
func TestExtractPPTXText_SparseNotesPairedBySuffix(t *testing.T) {
	const drawingMLNS = "http://schemas.openxmlformats.org/drawingml/2006/main"

	// Build a synthetic .pptx with three slide parts and only the
	// first and third notes parts populated.
	makeSlideXML := func(body string) []byte {
		return []byte(fmt.Sprintf(
			`<?xml version="1.0" encoding="UTF-8"?>`+
				`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`+
				` xmlns:a="%s">`+
				`<p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>%s</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld>`+
				`</p:sld>`,
			drawingMLNS, body,
		))
	}
	makeNotesXML := func(body string) []byte {
		return []byte(fmt.Sprintf(
			`<?xml version="1.0" encoding="UTF-8"?>`+
				`<p:notes xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`+
				` xmlns:a="%s">`+
				`<p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>%s</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld>`+
				`</p:notes>`,
			drawingMLNS, body,
		))
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := []struct {
		name string
		body []byte
	}{
		{"ppt/slides/slide1.xml", makeSlideXML("slide-one-body")},
		{"ppt/slides/slide2.xml", makeSlideXML("slide-two-body")},
		{"ppt/slides/slide3.xml", makeSlideXML("slide-three-body")},
		{"ppt/notesSlides/notesSlide1.xml", makeNotesXML("note-for-slide-one")},
		// notesSlide2.xml intentionally omitted (sparse notes).
		{"ppt/notesSlides/notesSlide3.xml", makeNotesXML("note-for-slide-three")},
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("zip.Create: %v", err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}

	got, err := extractPPTXText(buf.Bytes())
	if err != nil {
		t.Fatalf("extractPPTXText: %v", err)
	}

	// Slide 1 + its note, slide 2 alone, slide 3 + its note must
	// appear in that order with the notes attached to the correct
	// slides.
	mustOrder := []string{
		"slide-one-body",
		"note-for-slide-one",
		"slide-two-body",
		"slide-three-body",
		"note-for-slide-three",
	}
	prev := -1
	for _, want := range mustOrder {
		idx := strings.Index(got, want)
		if idx < 0 {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
		if idx <= prev {
			t.Fatalf("ordering broken at %q (idx=%d prev=%d): output:\n%s", want, idx, prev, got)
		}
		prev = idx
	}

	// Negative pin: notesSlide3 must NOT have been attached to
	// slide 2 by positional pairing. The note text must appear
	// AFTER slide-three-body, not directly after slide-two-body.
	slide2Idx := strings.Index(got, "slide-two-body")
	slide3Idx := strings.Index(got, "slide-three-body")
	note3Idx := strings.Index(got, "note-for-slide-three")
	if note3Idx < slide3Idx || note3Idx < slide2Idx {
		t.Fatalf("note for slide 3 appears in wrong position; got:\n%s", got)
	}
}
