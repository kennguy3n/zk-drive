package index

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractOpenDocumentText exercises the ODF extractor against
// a fixture produced by odfpy (different library than the in-house
// archive/zip + xml walker).
func TestExtractOpenDocumentText(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sample.odt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := extractOpenDocumentText(body)
	if err != nil {
		t.Fatalf("extractOpenDocumentText: %v", err)
	}

	for _, want := range []string{
		"Architecture Review",
		"row-level security",
		"café espresso menu",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in extracted text, got:\n%s", want, got)
		}
	}
}

func TestExtractOpenDocumentText_Malformed(t *testing.T) {
	_, err := extractOpenDocumentText([]byte("not a zip"))
	if err == nil {
		t.Fatal("expected error on non-zip input")
	}
}

// TestExtractOpenDocumentText_MissingContent exercises the archive-
// without-content.xml path. The extractor must surface a real
// error rather than silently writing empty content.
func TestExtractOpenDocumentText_MissingContent(t *testing.T) {
	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "no-content.zip")
	if err := writeMinimalZip(zipPath, "META-INF/manifest.xml", []byte("<m/>")); err != nil {
		t.Fatalf("writeMinimalZip: %v", err)
	}
	body, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	_, err = extractOpenDocumentText(body)
	if err == nil {
		t.Fatal("expected error on archive without content.xml")
	}
	if !strings.Contains(err.Error(), "missing content.xml") {
		t.Errorf("expected 'missing content.xml' error, got: %v", err)
	}
}

// TestParseODFContent_PreservesUserTypedTrailingSpacesInCells pins
// the correctness of the bytes.Buffer + pendingSyntheticSpace
// rework: a cell whose value legitimately ends in trailing spaces
// (e.g. `"product code   "`) must reach the FTS index intact, not
// stripped down to `"product code"` by the cell-close TrimRight
// the previous implementation used. The synthetic intra-cell
// separator we emit on </text:p> is dropped on </table-cell>, but
// every other trailing space (CharData, <text:s>) is preserved.
func TestParseODFContent_PreservesUserTypedTrailingSpacesInCells(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"
                        xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"
                        xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0">
  <office:body><office:spreadsheet><table:table>
    <table:table-row>
      <table:table-cell><text:p>product code   </text:p></table:table-cell>
      <table:table-cell><text:p>quantity</text:p></table:table-cell>
    </table:table-row>
  </table:table></office:spreadsheet></office:body>
</office:document-content>`
	got, err := parseODFContent(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("parseODFContent: %v", err)
	}
	// User-typed trailing spaces survive (followed by the cell's
	// own '\t' terminator). The synthetic space the </text:p>
	// close emitted is dropped \u2014 there is exactly ONE '\t'
	// between the cell content and the next cell, not "   \t" or
	// "\t".
	if !strings.Contains(got, "product code   \t") {
		t.Errorf("expected user-typed trailing spaces preserved before \\t, got: %q", got)
	}
	// The second cell exists and is correctly delimited.
	if !strings.Contains(got, "\tquantity\t") {
		t.Errorf("expected '\\tquantity\\t' (cell delimited on both sides), got: %q", got)
	}
}

// TestParseODFContent_MultiParagraphCellSeparator covers the
// counter-case to the trailing-spaces test: when a cell holds
// multiple paragraphs we DO want a synthetic space between them
// (for FTS phrase boundaries), but that synthetic space must
// disappear on cell close so the '\t' terminator sits flush.
func TestParseODFContent_MultiParagraphCellSeparator(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"
                        xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"
                        xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0">
  <office:body><office:spreadsheet><table:table>
    <table:table-row>
      <table:table-cell><text:p>line one</text:p><text:p>line two</text:p></table:table-cell>
    </table:table-row>
  </table:table></office:spreadsheet></office:body>
</office:document-content>`
	got, err := parseODFContent(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("parseODFContent: %v", err)
	}
	// Inter-paragraph space is present (FTS phrase boundary).
	if !strings.Contains(got, "line one line two") {
		t.Errorf("expected synthetic inter-paragraph space, got: %q", got)
	}
	// No awkward trailing-space-before-tab from the second
	// </text:p>.
	if strings.Contains(got, "line two \t") {
		t.Errorf("synthetic intra-cell space leaked into row terminator: %q", got)
	}
	if !strings.Contains(got, "line two\t") {
		t.Errorf("expected '\\t' immediately after last cell content, got: %q", got)
	}
}

// writeMinimalZip is a tiny test helper for building one-entry
// zip archives so extractor edge-case tests don't need to ship
// extra binary fixtures for trivial cases.
func writeMinimalZip(path, name string, payload []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := zip.NewWriter(f)
	entry, err := w.Create(name)
	if err != nil {
		return err
	}
	if _, err := entry.Write(payload); err != nil {
		return err
	}
	return w.Close()
}
