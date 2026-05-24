package preview

import (
	"context"
	"strings"
	"testing"
)

func TestRenderEmail_ValidRFC822(t *testing.T) {
	t.Parallel()
	raw := strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: Hello from the test suite",
		"Date: Mon, 25 May 2026 13:00:00 +0000",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"This is the body of the email.",
		"Multiple lines should render fine.",
	}, "\r\n")
	img, err := renderEmail(context.Background(), []byte(raw))
	if err != nil {
		t.Fatalf("renderEmail: %v", err)
	}
	if img == nil {
		t.Fatal("renderEmail returned nil image")
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("rendered image has empty bounds: %v", b)
	}
}

func TestRenderEmail_InvalidInput(t *testing.T) {
	t.Parallel()
	_, err := renderEmail(context.Background(), []byte("not even close to a valid email"))
	if err == nil {
		t.Fatal("expected error for invalid email")
	}
}

// TestRenderEmail_MboxFirstMessage ensures we extract the first
// RFC 5322 message out of an mbox archive and render it cleanly.
// Without the mbox-aware path, mail.ReadMessage would fail on the
// `From ` envelope line (which is not a valid RFC 5322 header).
func TestRenderEmail_MboxFirstMessage(t *testing.T) {
	t.Parallel()
	mbox := strings.Join([]string{
		"From alice@example.com Mon Jan  2 15:04:05 2006",
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: First message in mbox",
		"Date: Mon, 25 May 2026 13:00:00 +0000",
		"",
		"This is the body of the FIRST message.",
		"",
		"From bob@example.com Mon Jan  2 15:05:05 2006",
		"From: bob@example.com",
		"To: alice@example.com",
		"Subject: Second message in mbox (should NOT appear)",
		"Date: Mon, 25 May 2026 13:01:00 +0000",
		"",
		"This is the body of the SECOND message.",
	}, "\r\n")
	img, err := renderEmail(context.Background(), []byte(mbox))
	if err != nil {
		t.Fatalf("renderEmail on mbox: %v", err)
	}
	if img == nil {
		t.Fatal("renderEmail returned nil image on mbox input")
	}
	// And verify the extractor returns only the first message — the
	// second envelope must be excluded so the preview header stays
	// "First message in mbox", not garbled.
	got := extractFirstMboxMessage([]byte(mbox))
	if got == nil {
		t.Fatal("extractFirstMboxMessage returned nil for valid mbox input")
	}
	if strings.Contains(string(got), "Second message in mbox") {
		t.Fatalf("extractFirstMboxMessage should stop at the second envelope, got:\n%s", got)
	}
	if !strings.Contains(string(got), "First message in mbox") {
		t.Fatalf("extractFirstMboxMessage should preserve the first message, got:\n%s", got)
	}
}

// TestExtractFirstMboxMessage_NoFalsePositiveOnEML guards against the
// extractor mistaking a plain .eml file for mbox just because the
// first body line happens to start with "From " — RFC 5322 headers
// have the shape `Header: value` so anything starting with `From `
// without a colon is the mbox envelope, but a header line `From:`
// (with the colon) must NOT trigger the mbox path.
func TestExtractFirstMboxMessage_NoFalsePositiveOnEML(t *testing.T) {
	t.Parallel()
	eml := "From: alice@example.com\r\nSubject: hi\r\n\r\nbody\r\n"
	if got := extractFirstMboxMessage([]byte(eml)); got != nil {
		t.Fatalf("EML with `From:` header should NOT match mbox, got: %s", got)
	}
}

// TestRenderEmail_LongCJKSubject ensures we slice a long
// non-ASCII subject on a rune boundary so the preview doesn't
// render U+FFFD replacement glyphs. A byte-len cap at 63 would
// split the 3-byte UTF-8 sequence in the middle of "日"; this
// test would have caught that regression.
func TestRenderEmail_LongCJKSubject(t *testing.T) {
	t.Parallel()
	// 80 CJK runes (240 bytes) — well over the 64-rune cap.
	subject := strings.Repeat("日本語テストです", 10)
	raw := strings.Join([]string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: " + subject,
		"Date: Mon, 25 May 2026 13:00:00 +0000",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"body",
	}, "\r\n")
	img, err := renderEmail(context.Background(), []byte(raw))
	if err != nil {
		t.Fatalf("renderEmail: %v", err)
	}
	if img == nil {
		t.Fatal("renderEmail returned nil image")
	}
	// We can't easily extract the rendered subject from the pixel
	// data, but we CAN exercise the same truncation path the
	// renderer takes and assert it's clean. If a future refactor
	// breaks this assertion, the preview thumbnails would silently
	// regress to U+FFFD.
	got := truncateRunes(subject, 64, "…")
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("truncated subject contains U+FFFD: %q", got)
		}
	}
}

func TestStripMimeBoundaries(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"This is a preamble.",
		"--boundary-xyz",
		"Content-Type: text/plain",
		"",
		"plain part",
		"--boundary-xyz",
		"Content-Type: text/html",
		"",
		"<html>html part</html>",
		"--boundary-xyz--",
	}, "\n")
	got := stripMimeBoundaries(body)
	if strings.Contains(got, "--boundary-xyz") {
		t.Errorf("expected boundary lines to be stripped, got %q", got)
	}
	if strings.Contains(got, "Content-Type") {
		t.Errorf("expected per-part Content-Type headers to be stripped, got %q", got)
	}
	if !strings.Contains(got, "plain part") {
		t.Errorf("expected body content to be preserved, got %q", got)
	}
}

func TestStripMimeBoundaries_RealWorldPrefixes(t *testing.T) {
	t.Parallel()
	// Real Outlook / Java-mail / Exchange boundaries use 3+ leading
	// dashes. An earlier heuristic explicitly skipped these via
	// `!HasPrefix("---")` which is exactly the opposite of what we
	// want. Anchor that bug with a regression test so it can't
	// silently come back.
	cases := []string{
		"------=_NextPart_000_001D",
		"----=_Part_12345_678",
		"--===============0123456789--",
		"--_000_BOUNDARY_NAME_",
	}
	for _, boundary := range cases {
		body := strings.Join([]string{
			"preamble",
			boundary,
			"Content-Type: text/plain",
			"",
			"part body",
			boundary + "--",
		}, "\n")
		got := stripMimeBoundaries(body)
		if strings.Contains(got, boundary) {
			t.Errorf("boundary %q should have been stripped from %q", boundary, got)
		}
	}
}

func TestStripMimeBoundaries_PreservesMarkdownRule(t *testing.T) {
	t.Parallel()
	// A plain "---" line is a markdown horizontal rule, not a MIME
	// boundary. The heuristic must NOT swallow it (and the lines
	// after it).
	body := strings.Join([]string{
		"Hi there,",
		"",
		"---",
		"",
		"Yours truly,",
	}, "\n")
	got := stripMimeBoundaries(body)
	if !strings.Contains(got, "---") {
		t.Errorf("markdown rule should be preserved, got %q", got)
	}
	if !strings.Contains(got, "Yours truly") {
		t.Errorf("content after markdown rule should be preserved, got %q", got)
	}
}
