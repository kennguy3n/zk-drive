package email

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRenderGuestInvite_TextBody asserts the plain-text template
// reproduces every required field verbatim. We do NOT lock the
// whole body string (would be brittle under copyediting); instead
// we assert the data-bearing tokens land in the output.
func TestRenderGuestInvite_TextBody(t *testing.T) {
	exp := time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC)
	data := GuestInviteData{
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		Email:         "bob@example.com",
		AcceptURL:     "https://drive.example.com/invites/INVITEID",
		ExpiresAt:     exp.Format("2006-01-02 15:04 UTC"),
	}
	pair, err := renderGuestInvite(data)
	if err != nil {
		t.Fatalf("renderGuestInvite: %v", err)
	}
	for name, needle := range map[string]string{
		"inviter":      "Alice",
		"workspace":    "Acme Co",
		"folder":       "Q4 Roadmap",
		"role":         "editor",
		"email":        "bob@example.com",
		"accept_url":   "https://drive.example.com/invites/INVITEID",
		"expires_at":   "2025-12-31 23:59 UTC",
	} {
		if !strings.Contains(pair.Text, needle) {
			t.Errorf("text body missing %s (%q):\n%s", name, needle, pair.Text)
		}
	}
}

// TestRenderGuestInvite_HTMLAutoEscape pins the html/template
// auto-escape guarantee: a user-controlled field (here, FolderName)
// must not be able to inject raw HTML / JS into the rendered body.
// If a future contributor swaps html/template for text/template
// this test catches the regression immediately.
func TestRenderGuestInvite_HTMLAutoEscape(t *testing.T) {
	data := GuestInviteData{
		InviterName:   "Mallory",
		WorkspaceName: "Acme Co",
		FolderName:    `<script>alert("xss")</script>`,
		Role:          "viewer",
		Email:         "bob@example.com",
		AcceptURL:     "https://drive.example.com/invites/X",
	}
	pair, err := renderGuestInvite(data)
	if err != nil {
		t.Fatalf("renderGuestInvite: %v", err)
	}
	if strings.Contains(pair.HTML, "<script>") {
		t.Fatalf("html body contains unescaped <script>: %s", pair.HTML)
	}
	if !strings.Contains(pair.HTML, "&lt;script&gt;") {
		t.Fatalf("html body did not escape <script> to &lt;script&gt;:\n%s", pair.HTML)
	}
}

// TestRenderGuestInvite_MissingFieldsRejected asserts the
// pre-render validator catches missing required fields before we
// try to send a half-blank email.
func TestRenderGuestInvite_MissingFieldsRejected(t *testing.T) {
	cases := []struct {
		name  string
		mutate func(*GuestInviteData)
		want   string
	}{
		{"InviterName", func(d *GuestInviteData) { d.InviterName = "" }, "InviterName"},
		{"WorkspaceName", func(d *GuestInviteData) { d.WorkspaceName = "" }, "WorkspaceName"},
		{"FolderName", func(d *GuestInviteData) { d.FolderName = "" }, "FolderName"},
		{"Role", func(d *GuestInviteData) { d.Role = "" }, "Role"},
		{"Email", func(d *GuestInviteData) { d.Email = "" }, "Email"},
		{"AcceptURL", func(d *GuestInviteData) { d.AcceptURL = "" }, "AcceptURL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := GuestInviteData{
				InviterName:   "A",
				WorkspaceName: "W",
				FolderName:    "F",
				Role:          "viewer",
				Email:         "x@y",
				AcceptURL:     "https://x/y",
			}
			tc.mutate(&data)
			_, err := renderGuestInvite(data)
			if err == nil {
				t.Fatalf("expected error for missing %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to mention %s, got %q", tc.want, err.Error())
			}
		})
	}
}

// TestRenderGuestInvite_ExpiresAtOmitted asserts the templates
// drop the expiry line entirely when the field is empty. Both
// templates use the same `{{- if .ExpiresAt}}` guard so the test
// covers both code paths via the rendered output.
func TestRenderGuestInvite_ExpiresAtOmitted(t *testing.T) {
	data := GuestInviteData{
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		Email:         "bob@example.com",
		AcceptURL:     "https://drive.example.com/invites/X",
	}
	pair, err := renderGuestInvite(data)
	if err != nil {
		t.Fatalf("renderGuestInvite: %v", err)
	}
	if strings.Contains(pair.Text, "Expires:") {
		t.Fatalf("text body should not contain Expires: when ExpiresAt is empty:\n%s", pair.Text)
	}
	if strings.Contains(pair.HTML, "Expires") {
		t.Fatalf("html body should not contain Expires when ExpiresAt is empty:\n%s", pair.HTML)
	}
}

// TestTemplatesParsedAtInit pins the parse-at-package-init
// contract: both guest-invite templates must be non-nil after
// the package's init phase. A malformed embedded template would
// cause texttemplate.Must / htmltemplate.Must to panic during
// init, which the Go test runtime surfaces as a package-load
// failure — so the assertion below mainly documents the contract
// for future readers. If a maintainer accidentally moves the
// parse back into the per-render hot path, this test still
// passes (the var would just be the parsed template), so it is
// paired with TestRenderGuestInvite_ConcurrentSafe below to
// catch the regression a different way.
//
// Regression test for the architectural fix to ANALYSIS_0007
// from the fourth Devin Review pass on PR #66.
func TestTemplatesParsedAtInit(t *testing.T) {
	if guestInviteTextTmpl == nil {
		t.Fatal("guestInviteTextTmpl is nil — parse-at-init contract broken")
	}
	if guestInviteHTMLTmpl == nil {
		t.Fatal("guestInviteHTMLTmpl is nil — parse-at-init contract broken")
	}
	// Sanity-check the parsed names so a future refactor that
	// renames the template files doesn't silently keep working
	// with empty templates.
	if guestInviteTextTmpl.Name() == "" {
		t.Errorf("guestInviteTextTmpl has empty name")
	}
	if guestInviteHTMLTmpl.Name() == "" {
		t.Errorf("guestInviteHTMLTmpl has empty name")
	}
}

// TestRenderGuestInvite_ConcurrentSafe drives many goroutines
// through renderGuestInvite simultaneously. Because the templates
// are parsed at package init and only Execute (which is documented
// concurrency-safe in both text/template and html/template) runs
// per call, this is expected to be race-free under `go test -race`.
// If a future refactor reintroduces per-call parsing on a shared
// *Template, the race detector will catch the unsynchronised
// AST mutation.
func TestRenderGuestInvite_ConcurrentSafe(t *testing.T) {
	data := GuestInviteData{
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		Email:         "bob@example.com",
		AcceptURL:     "https://drive.example.com/invites/X",
	}
	const goroutines = 32
	const itersPerGoroutine = 16
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*itersPerGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < itersPerGoroutine; j++ {
				pair, err := renderGuestInvite(data)
				if err != nil {
					errs <- err
					return
				}
				if !strings.Contains(pair.Text, "Acme Co") {
					errs <- nil // signal completion without err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent render: %v", err)
		}
	}
}
