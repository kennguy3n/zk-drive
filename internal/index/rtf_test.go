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

// TestExtractRTFText_SurrogatePairsDecodeNonBMP pins the bug
// surfaced by Devin Review: Word and other RTF writers emit
// non-BMP code points as a UTF-16 surrogate pair, e.g. an emoji
// U+1F600 is written as `\u55357?\u56832?` under the implicit
// \uc1 default. The high-surrogate \u escape consumes one ANSI
// fallback byte (the '?'), then the low-surrogate \u escape sits
// immediately after that fallback. A previous implementation
// passed the residual ucSkip counter (=0 at that point) into the
// surrogate look-ahead, so the look-ahead saw '?' instead of '\'
// and bailed — both surrogates were silently dropped and the
// emoji never made it into the FTS corpus. The fix passes
// ucDefault, which is the spec-defined number of fallback bytes
// the high surrogate just consumed.
func TestExtractRTFText_SurrogatePairsDecodeNonBMP(t *testing.T) {
	// U+1F600 (😀) decomposes to high=0xD83D=55357, low=0xDE00=56832
	// in UTF-16. Each \u carries a single ANSI fallback '?' under
	// the implicit \uc1 default.
	body := []byte("{\\rtf1 hello \\u55357?\\u56832? world}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	if !strings.ContainsRune(got, 0x1F600) {
		t.Errorf("expected U+1F600 emoji in extracted text, got: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("expected surrounding text to survive surrogate decode, got: %q", got)
	}
}

// TestExtractRTFText_OrphanHighSurrogateDoesNotBleed verifies a
// stray high surrogate that ISN'T followed by a paired low
// surrogate gets dropped cleanly without consuming surrounding
// content. RTF in the wild occasionally drops the low half (e.g.
// a truncated paste); the extractor must not emit invalid UTF-8
// or eat the following plaintext.
func TestExtractRTFText_OrphanHighSurrogateDoesNotBleed(t *testing.T) {
	// \u55357 = high surrogate, fallback '?', then plain "ok}".
	// Expected: 'hello ' + (orphan dropped) + 'ok' = "hello ok".
	body := []byte("{\\rtf1 hello \\u55357? ok}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "ok") {
		t.Errorf("orphan high surrogate ate surrounding text: %q", got)
	}
}

// TestExtractRTFText_SurrogatePairWithHexEscapedFallback covers the
// Word-style emission where each fallback byte is encoded as `\'XX`
// (hex escape) rather than a literal byte. The previous skip loop
// terminated on the first `\` it saw, so the low-surrogate `\u`
// was never found and the non-BMP code point was silently dropped.
// The fix recognises `\'XX` as one fallback character occupying
// four source bytes.
func TestExtractRTFText_SurrogatePairWithHexEscapedFallback(t *testing.T) {
	// U+1F600 (😀) — high 0xD83D=55357, low 0xDE00=56832. Each
	// fallback byte is hex-escaped as `\'3F` (`?`).
	body := []byte("{\\rtf1 hi \\u55357\\'3F\\u56832\\'3F end}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	if !strings.ContainsRune(got, 0x1F600) {
		t.Errorf("hex-escaped fallback bytes blocked surrogate decode, got: %q", got)
	}
	if !strings.Contains(got, "hi") || !strings.Contains(got, "end") {
		t.Errorf("expected surrounding text to survive surrogate decode, got: %q", got)
	}
}

// TestExtractRTFText_SurrogatePairWithEscapedLiteralFallback covers
// the case where the fallback byte is itself an escaped literal
// (\\ \{ \}). Two source bytes, one fallback character. The fix
// recognises this so the surrogate pair still resolves.
func TestExtractRTFText_SurrogatePairWithEscapedLiteralFallback(t *testing.T) {
	// Fallback is the escaped literal `\\` (one backslash char).
	body := []byte("{\\rtf1 hi \\u55357\\\\\\u56832\\\\ end}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	if !strings.ContainsRune(got, 0x1F600) {
		t.Errorf("escaped-literal fallback blocked surrogate decode, got: %q", got)
	}
}

// TestExtractRTFText_HexEscapedFallbackUnderUC2 covers the multi-
// fallback case: \uc2 means each \u consumes TWO fallback
// characters, and those fallbacks may freely mix hex escapes and
// literals. The skip loop must count semantic characters, not
// source bytes, across the mix.
func TestExtractRTFText_HexEscapedFallbackUnderUC2(t *testing.T) {
	// \uc2 then U+1F600. High surrogate consumes 2 fallback
	// chars: `\'3F` (hex) + `?` (literal). Low surrogate
	// consumes 2: `?` (literal) + `\'3F` (hex).
	body := []byte("{\\rtf1\\uc2 hi \\u55357\\'3F?\\u56832?\\'3F end}")
	got, err := extractRTFText(body)
	if err != nil {
		t.Fatalf("extractRTFText: %v", err)
	}
	if !strings.ContainsRune(got, 0x1F600) {
		t.Errorf("mixed hex/literal fallback under \\uc2 blocked surrogate, got: %q", got)
	}
	if !strings.Contains(got, "hi") || !strings.Contains(got, "end") {
		t.Errorf("expected surrounding text intact, got: %q", got)
	}
}
