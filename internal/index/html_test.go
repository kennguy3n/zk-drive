package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractHTMLText pins the contract for HTML extraction:
//
//   - body text is extracted
//   - <head>, <title>, <style>, <script> content is skipped
//   - inline whitespace is collapsed (multiple spaces → one)
//   - <pre> preserves interior whitespace
//   - block-level closes flush newlines so phrase queries don't
//     span structural boundaries
func TestExtractHTMLText(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sample.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := extractHTMLText(body)
	if err != nil {
		t.Fatalf("extractHTMLText: %v", err)
	}

	for _, want := range []string{
		"Engineering Update",
		"release ships several improvements", // collapsed whitespace
		"Faster startup",
		"Better logging",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in extracted text, got:\n%s", want, got)
		}
	}

	// <pre> preserves interior whitespace.
	if !strings.Contains(got, "  preserved") {
		t.Errorf("expected <pre> to preserve indent, got:\n%s", got)
	}

	// <script>, <style>, <head> content must NOT leak.
	for _, banned := range []string{
		"Should not appear", // <title>
		"color: red",        // <style>
		"hide me",           // <script>
	} {
		if strings.Contains(got, banned) {
			t.Errorf("non-body content leaked: %q in:\n%s", banned, got)
		}
	}
}

func TestExtractHTMLText_Empty(t *testing.T) {
	_, err := extractHTMLText(nil)
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}

// TestExtractHTMLText_PartialHTML surfaces the permissive-parser
// behaviour: a fragment without <html>/<body> wrappers must still
// extract its text content.
func TestExtractHTMLText_PartialHTML(t *testing.T) {
	got, err := extractHTMLText([]byte("<p>Hello, world!</p>"))
	if err != nil {
		t.Fatalf("extractHTMLText fragment: %v", err)
	}
	if !strings.Contains(got, "Hello, world!") {
		t.Errorf("expected fragment text, got:\n%s", got)
	}
}
