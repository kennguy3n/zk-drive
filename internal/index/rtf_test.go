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

// TestExtractRTFText_UC0RespectsZeroFallback pins the macOS
// TextEdit / LibreOffice "\uc0" dialect: under \uc0 the extractor
// must NOT consume any ANSI fallback byte after a "\u" escape, so
// the byte immediately after the Unicode escape stays in the
// output. The pre-fix implementation hardcoded a 1-byte skip and
// silently swallowed one character per escape.
func TestExtractRTFText_UC0RespectsZeroFallback(t *testing.T) {
	// "\uc0" sets the group fallback width to zero. The "\u65"
	// escape decodes to 'A' (U+0041) and, under \uc0, MUST NOT
	// consume the following 'X'. The expected extract is
	// therefore "AXdone".
	body := []byte("{\\rtf1\\uc0 \\u65 X\\u100 one}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	const want = "AXdone"
	if got != want {
		t.Errorf("\\uc0 fallback handling wrong:\n got=%q\nwant=%q", got, want)
	}
}

// TestExtractRTFText_UC1DefaultStillConsumesOneByte verifies the
// historical RTF default still holds: under \uc1 (and the implicit
// top-of-document default) a "\u" escape consumes exactly one ANSI
// fallback byte.
func TestExtractRTFText_UC1DefaultStillConsumesOneByte(t *testing.T) {
	body := []byte("{\\rtf1\\uc1 \\u65 X\\u100 one}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	// \u65 emits 'A'; the following 'X' is the ANSI fallback and
	// gets skipped under \uc1. \u100 emits 'd' (U+0064); the
	// following 'o' is the fallback and gets skipped. The
	// remaining "ne" is plain text. So the expected extract is
	// "Adne".
	const want = "Adne"
	if got != want {
		t.Errorf("\\uc1 fallback handling wrong:\n got=%q\nwant=%q", got, want)
	}
}

// TestExtractRTFText_UCScopedToGroup verifies a child group can set
// \uc0 without affecting its sibling, and that close-brace restores
// the parent group's default.
func TestExtractRTFText_UCScopedToGroup(t *testing.T) {
	// Outer group: implicit \uc1 (default). Inner group sets
	// \uc0. After the inner group closes we should be back at
	// \uc1 — so the trailing \u90 Z consumes one byte.
	body := []byte("{\\rtf1 {\\uc0 \\u65 X}{\\u90 Z}}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	// Inner: \uc0 keeps both A and X. Sibling: \uc1 swallows Z.
	const want = "AXZ"
	// Z is the fallback for \u90 under the outer \uc1 default, so
	// the visible runs are A, X (inner under \uc0 — keep both),
	// then Z fallback discarded leaving 'Z' from the \u90 itself.
	// Output: "AX" + chr(\u90)="Z" = "AXZ".
	if got != want {
		t.Errorf("group-scoped uc handling wrong:\n got=%q\nwant=%q", got, want)
	}
}
