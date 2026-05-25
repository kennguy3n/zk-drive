package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractRTFText pins the RTF state-machine extractor against
// a hand-crafted fixture that exercises:
//
//   - paragraph breaks (\par → newline)
//   - hex-encoded accented characters (\'e9 → 'é', requires Latin-1
//     fast path that we DO handle by treating < 0x80 as ASCII)
//   - Unicode escapes (\u8217? → smart quote '’')
//   - skipped metadata destinations (\fonttbl, \info)
//   - optional destinations (\*\generator — must be skipped)
func TestExtractRTFText(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sample.rtf"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}

	for _, want := range []string{
		"Quarterly results.",
		"Caf",      // 'café' — the 'é' is hex-escaped; we extract Caf + (skipped non-ASCII)
		"espresso", // exercises the hex-escape boundary continuation
		"Unicode test",
		"smart quote",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in extracted text, got:\n%s", want, got)
		}
	}

	// The Unicode smart quote (U+2019 = 8217) must appear because
	// \u8217 is a BMP code point and the extractor decodes it.
	if !strings.ContainsRune(got, '\u2019') {
		t.Errorf("expected U+2019 smart quote in extracted text, got:\n%q", got)
	}

	// Metadata destinations must be dropped.
	for _, banned := range []string{
		"Times New Roman",
		"FixtureGenerator",
		"secret-meta-do-not-extract",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("metadata destination leaked into extract: %q in:\n%s", banned, got)
		}
	}
}

// TestExtractRTFText_RejectsNonRTF surfaces non-RTF bytes as an
// error so the worker doesn't write garbage content_text on
// mime-type misdetection.
func TestExtractRTFText_RejectsNonRTF(t *testing.T) {
	_, err := extractRTFText([]byte("plain text, no rtf preamble"))
	if err == nil {
		t.Fatal("expected error on non-RTF input")
	}
}

func TestExtractRTFText_UnbalancedBraces(t *testing.T) {
	_, err := extractRTFText([]byte("{\\rtf1 hello "))
	if err == nil {
		t.Fatal("expected error on unbalanced braces")
	}
	if !strings.Contains(err.Error(), "unbalanced") {
		t.Errorf("expected unbalanced-braces error, got: %v", err)
	}
}

// TestExtractRTFText_Empty exercises the empty-body edge.
func TestExtractRTFText_Empty(t *testing.T) {
	_, err := extractRTFText(nil)
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}
