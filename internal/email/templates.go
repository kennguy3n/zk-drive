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
// template parse or execute fails — callers that hit a render
// error MUST NOT call Send (the metric path classifies render
// failures as `template_error`).
func renderGuestInvite(data GuestInviteData) (renderedPair, error) {
	if err := validateGuestInvite(data); err != nil {
		return renderedPair{}, err
	}
	text, err := executeTextTemplate("templates/guest_invite.txt.tmpl", data)
	if err != nil {
		return renderedPair{}, err
	}
	html, err := executeHTMLTemplate("templates/guest_invite.html.tmpl", data)
	if err != nil {
		return renderedPair{}, err
	}
	return renderedPair{Text: text, HTML: html}, nil
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

func executeTextTemplate(path string, data any) (string, error) {
	body, err := templateFS.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("email: read template %s: %w", path, err)
	}
	tmpl, err := texttemplate.New(path).Parse(string(body))
	if err != nil {
		return "", fmt.Errorf("email: parse text template %s: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("email: execute text template %s: %w", path, err)
	}
	return buf.String(), nil
}

func executeHTMLTemplate(path string, data any) (string, error) {
	body, err := templateFS.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("email: read template %s: %w", path, err)
	}
	tmpl, err := htmltemplate.New(path).Parse(string(body))
	if err != nil {
		return "", fmt.Errorf("email: parse html template %s: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("email: execute html template %s: %w", path, err)
	}
	return buf.String(), nil
}
