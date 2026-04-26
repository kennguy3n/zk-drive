package index

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestExtractTextUTF8BoundaryTruncation builds a body whose byte
// length straddles MaxIndexBytes with a multi-byte rune. Without the
// rune-boundary trim, the returned string would be invalid UTF-8 and
// Postgres would reject the content_text write, which sends the
// worker into a Nak / redeliver loop. The test asserts ExtractText
// returns a string that is both under the cap and valid UTF-8.
func TestExtractTextUTF8BoundaryTruncation(t *testing.T) {
	const rune4 = "\U0001F600" // 😀 — 4 bytes in UTF-8
	// Fill with ASCII up to MaxIndexBytes-2, then a 4-byte rune, so
	// the rune crosses the MaxIndexBytes boundary by 2 bytes.
	padding := strings.Repeat("a", int(MaxIndexBytes)-2)
	body := []byte(padding + rune4 + strings.Repeat("b", 128))

	got, err := ExtractText("text/plain", body)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}
	if int64(len(got)) > MaxIndexBytes {
		t.Fatalf("exceeded cap: len=%d max=%d", len(got), MaxIndexBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("returned invalid UTF-8")
	}
	// The trailing 4-byte rune must have been dropped — if any of
	// its bytes leaked through the result would fail ValidString,
	// but also verify the last character is plain ASCII.
	last, size := utf8.DecodeLastRuneInString(got)
	if size != 1 || last != 'a' {
		t.Fatalf("unexpected tail rune: %q (size=%d)", last, size)
	}
}

// TestExtractTextJSONXMLTruncation ensures the JSON / XML branches
// inherit the same rune-boundary trim as text/*.
func TestExtractTextJSONXMLTruncation(t *testing.T) {
	const rune4 = "\U0001F600"
	padding := strings.Repeat("x", int(MaxIndexBytes)-1)
	body := []byte(padding + rune4)
	for _, mt := range []string{"application/json", "application/xml"} {
		got, err := ExtractText(mt, body)
		if err != nil {
			t.Fatalf("%s: %v", mt, err)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("%s: invalid UTF-8 at boundary", mt)
		}
		if int64(len(got)) > MaxIndexBytes {
			t.Fatalf("%s: exceeded cap: len=%d", mt, len(got))
		}
	}
}

func TestExtractTextShortBodyPassthrough(t *testing.T) {
	got, err := ExtractText("text/plain", []byte("hello 😀 world"))
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}
	if got != "hello 😀 world" {
		t.Fatalf("unexpected body: %q", got)
	}
}

func TestExtractTextUnsupported(t *testing.T) {
	if _, err := ExtractText("image/png", []byte{0x89, 0x50}); err == nil {
		t.Fatal("expected unsupported mime error")
	}
	if _, err := ExtractText("", []byte("x")); err == nil {
		t.Fatal("expected unsupported mime error for empty type")
	}
}
