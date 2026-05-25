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
