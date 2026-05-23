package index

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// buildMinimalDOCX returns the bytes of a valid .docx archive whose
// body contains the supplied paragraphs in order. Each paragraph
// becomes a <w:p> with one <w:r><w:t> run. The result is a
// well-formed OOXML zip that pure-Go extractDOCXText can parse —
// no external `docx`-generating tool is needed.
//
// Other entries that a "real" .docx ships (Content_Types.xml,
// _rels/, app metadata) are intentionally omitted: the extractor
// only cares about word/document.xml. Tests for malformed-zip and
// missing-entry paths build the archive separately.
func buildMinimalDOCX(t *testing.T, paragraphs []string) []byte {
	t.Helper()

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sb.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`)
	sb.WriteString(`<w:body>`)
	for _, p := range paragraphs {
		sb.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
		// Caller is responsible for not embedding "<" / "&" in p
		// (we use simple text fixtures); the test would catch any
		// regression because the decoder would reject malformed XML.
		sb.WriteString(p)
		sb.WriteString(`</w:t></w:r></w:p>`)
	}
	sb.WriteString(`</w:body></w:document>`)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create entry: %v", err)
	}
	if _, err := w.Write([]byte(sb.String())); err != nil {
		t.Fatalf("zip write entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractDOCXText(t *testing.T) {
	t.Parallel()
	docx := buildMinimalDOCX(t, []string{
		"Quarterly report",
		"Revenue exceeded forecast by twelve percent.",
	})

	got, err := extractDOCXText(docx)
	if err != nil {
		t.Fatalf("extractDOCXText: %v", err)
	}
	for _, want := range []string{"Quarterly report", "Revenue exceeded forecast", "twelve percent"} {
		if !strings.Contains(got, want) {
			t.Fatalf("extracted text missing %q; got %q", want, got)
		}
	}
	// Paragraphs should be separated by newlines so FTS phrase
	// queries don't accidentally span paragraph boundaries.
	if !strings.Contains(got, "report\nRevenue") {
		t.Fatalf("expected paragraph newline between body lines; got %q", got)
	}
}

func TestExtractDOCXTextHandlesTabAndLineBreak(t *testing.T) {
	t.Parallel()
	// Build a doc that uses <w:tab/> and <w:br/> mid-paragraph. The
	// extractor must render them as \t and \n so the original visual
	// structure survives into FTS-indexable form.
	xml := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` +
		`<w:p><w:r><w:t>Col1</w:t><w:tab/><w:t>Col2</w:t><w:br/><w:t>Row2</w:t></w:r></w:p>` +
		`</w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte(xml))
	_ = zw.Close()

	got, err := extractDOCXText(buf.Bytes())
	if err != nil {
		t.Fatalf("extractDOCXText: %v", err)
	}
	if got != "Col1\tCol2\nRow2\n" {
		// Trailing newline comes from the closing </w:p>. The exact
		// shape is what FTS would see: Col1<TAB>Col2<NL>Row2<NL>.
		t.Fatalf("unexpected tab/br rendering: %q", got)
	}
}

func TestExtractDOCXTextRejectsNonZipInput(t *testing.T) {
	t.Parallel()
	// A blob that is not a zip archive must surface a non-Unsupported
	// error so the worker re-delivers and the operator can see the
	// real failure. Mirrors internal/preview/pdf_test.go's invariant
	// that invalid input must not masquerade as ErrUnsupportedMime.
	_, err := extractDOCXText([]byte("not a docx, just bytes"))
	if err == nil {
		t.Fatal("expected error for non-zip input")
	}
	if errors.Is(err, ErrUnsupportedMimeType) {
		t.Fatalf("invalid DOCX should not return ErrUnsupportedMimeType, got %v", err)
	}
}

func TestExtractDOCXTextRejectsZipMissingDocumentEntry(t *testing.T) {
	t.Parallel()
	// A valid zip with no word/document.xml is not a usable .docx.
	// Surface a real error rather than silently returning "".
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("not-document.xml")
	_, _ = w.Write([]byte("<root/>"))
	_ = zw.Close()

	_, err := extractDOCXText(buf.Bytes())
	if err == nil {
		t.Fatal("expected error when word/document.xml is missing")
	}
	if errors.Is(err, ErrUnsupportedMimeType) {
		t.Fatalf("missing entry should not return ErrUnsupportedMimeType, got %v", err)
	}
}

func TestExtractDOCXTextRejectsMalformedBodyXML(t *testing.T) {
	t.Parallel()
	// Body XML that is syntactically invalid must NOT be treated as
	// an unsupported type — it is a real failure that the worker
	// should redeliver and log.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte("<w:document><unclosed></w:document>"))
	_ = zw.Close()

	_, err := extractDOCXText(buf.Bytes())
	if err == nil {
		t.Fatal("expected error for malformed body xml")
	}
	if errors.Is(err, ErrUnsupportedMimeType) {
		t.Fatalf("malformed body xml should not return ErrUnsupportedMimeType, got %v", err)
	}
}

func TestExtractTextRoutesDOCXThroughDOCXExtractor(t *testing.T) {
	t.Parallel()
	docx := buildMinimalDOCX(t, []string{"Hello DOCX body"})

	got, err := ExtractText(
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		docx,
	)
	if err != nil {
		t.Fatalf("ExtractText(docx): %v", err)
	}
	if !strings.Contains(got, "Hello DOCX body") {
		t.Fatalf("ExtractText routed DOCX but body missing; got %q", got)
	}
}
