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
