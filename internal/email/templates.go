package email

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// Embedded templates are parsed exactly once at package init via
// the canonical text/template + html/template Must(ParseFS(...))
// pattern. Doing so:
//
//   - Eliminates the per-send filesystem read + AST allocation
//     cost that an executor-per-call shape pays. The embed.FS
//     read is in-memory and cheap on its own, but the parse
//     allocates a fresh action tree, function map, and node
//     graph each time. At enterprise volumes (batch-invite,
//     password-reset, future MFA-disabled notifications) the
//     waste compounds.
//   - Surfaces malformed embedded templates as a startup panic
//     instead of a first-render error. A parse failure on a
//     checked-in template is a build-time mistake — operators
//     should learn about it from the binary refusing to start,
//     not from a downstream email-send returning a 500.
//   - Allows concurrent calls to .Execute without locking. Both
//     text/template and html/template document Execute as safe
//     for concurrent use; a fresh-parse-per-call shape gives up
//     that guarantee for no reason.
//
// If a future feature adds operator-supplied or hot-reloaded
// templates, they should live in a separate registry — the
// embedded set stays parse-at-init.
var (
	guestInviteTextTmpl = texttemplate.Must(texttemplate.ParseFS(templateFS, "templates/guest_invite.txt.tmpl"))
	guestInviteHTMLTmpl = htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/guest_invite.html.tmpl"))
)

// GuestInviteData is the strongly-typed payload for the
// guest-invite email pair. Fields are intentionally pre-rendered
// strings (ExpiresAt formatted via time.Format, AcceptURL fully
// composed) so the template stays declarative and the call-site
// owns presentation policy (timezone, URL scheme, etc.).
type GuestInviteData struct {
	InviterName   string
	WorkspaceName string
	FolderName    string
	Role          string
	Email         string
	AcceptURL     string
	// ExpiresAt is the human-readable expiry string. Empty when
	// the invite has no expiry.
	ExpiresAt string
}

// renderedPair holds the text + html bodies for a single
// templated email. The HTML may be empty when only a text part
// is required (e.g. plain operational notices) — the SMTP layer
// downgrades to a single text/plain part when HTMLBody == "".
type renderedPair struct {
	Text string
	HTML string
}

// renderGuestInvite executes both halves of the guest-invite
// template against the supplied data. Returns an error if either
// template execute fails — callers that hit a render error MUST
// NOT call Send (the metric path classifies render failures as
// `template_error`).
//
// The templates themselves are parsed once at package init (see
// the package-level guestInvite{Text,HTML}Tmpl vars), so this
// function only does the validate + execute work — no parse, no
// FS read.
func renderGuestInvite(data GuestInviteData) (renderedPair, error) {
	if err := validateGuestInvite(data); err != nil {
		return renderedPair{}, err
	}
	var textBuf bytes.Buffer
	if err := guestInviteTextTmpl.Execute(&textBuf, data); err != nil {
		return renderedPair{}, fmt.Errorf("email: execute text template: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := guestInviteHTMLTmpl.Execute(&htmlBuf, data); err != nil {
		return renderedPair{}, fmt.Errorf("email: execute html template: %w", err)
	}
	return renderedPair{Text: textBuf.String(), HTML: htmlBuf.String()}, nil
}

func validateGuestInvite(d GuestInviteData) error {
	missing := []string{}
	if strings.TrimSpace(d.InviterName) == "" {
		missing = append(missing, "InviterName")
	}
	if strings.TrimSpace(d.WorkspaceName) == "" {
		missing = append(missing, "WorkspaceName")
	}
	if strings.TrimSpace(d.FolderName) == "" {
		missing = append(missing, "FolderName")
	}
	if strings.TrimSpace(d.Role) == "" {
		missing = append(missing, "Role")
	}
	if strings.TrimSpace(d.Email) == "" {
		missing = append(missing, "Email")
	}
	if strings.TrimSpace(d.AcceptURL) == "" {
		missing = append(missing, "AcceptURL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("email: guest invite missing required template fields: %s", strings.Join(missing, ", "))
	}
	return nil
}
