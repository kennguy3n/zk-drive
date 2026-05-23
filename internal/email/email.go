// Package email is the transactional-email surface for ZK Drive.
//
// Today it powers guest-invite delivery (api/drive/sharing.go's
// CreateGuestInvite path) — historically a TODO in the codebase: a
// workspace owner could invite an external address but the invitee
// was never notified. The same Service interface is used everywhere
// the server needs to send a templated email to a known address
// (future password-reset, MFA-disabled notice, share-link "you've
// been mentioned" etc.).
//
// Why an interface (not just a concrete SMTPClient)
//
//   - Unit tests assert template output and call-site wiring without
//     standing up a real SMTP server.
//   - Local dev (no SMTP relay handy) keeps booting against the
//     NoopClient and surfaces a clear warning at startup so the
//     operator notices the missing config before production traffic
//     lands.
//
// Why net/smtp (no third-party SaaS dependency)
//
// Every reasonable transactional-email provider (Postmark, Mailgun,
// AWS SES, Gmail App Passwords, corporate Exchange relays) speaks
// SMTP-AUTH with PLAIN/LOGIN over STARTTLS or implicit TLS. Standing
// up a wire-level client keeps the operator free to choose the
// provider without forcing us to vendor that provider's SDK. The
// trade-off (no per-message reporting webhooks, no template editor
// UI) is fine for the volume we expect — single-digit-millions per
// month max, well under any provider's SMTP rate limit.
package email

import (
	"context"
	"errors"
)

// ErrNotConfigured is returned by Service.Send when no SMTP relay is
// wired (NoopClient mode). Callers treat it as a soft failure: log,
// emit the disabled-counter metric, and continue. Sharing flows do
// NOT abort the underlying database write when delivery is
// unavailable — the invite row already exists and an operator can
// re-notify out-of-band.
var ErrNotConfigured = errors.New("email: transactional relay not configured")

// Message is a single rendered outbound email. RecipientName is
// optional and gets formatted into the To header when present
// ("Alice <alice@example.com>"). HTMLBody is also optional — when
// empty the transport sends a single text/plain part instead of a
// multipart/alternative. Headers maps arbitrary headers (e.g.
// "Auto-Submitted: auto-generated" for system notices) onto the
// outgoing message; standard headers (From/To/Subject/Date/MIME-*
// /Message-ID) are managed by the transport.
type Message struct {
	To             string
	RecipientName  string
	Subject        string
	TextBody       string
	HTMLBody       string
	Headers        map[string]string
	TemplateName   string // observability label, NOT a header
}

// Sender is the abstraction the rest of the server uses. The
// concrete SMTPClient + NoopClient both implement it; tests provide
// in-memory fakes (see service_test.go) for golden-file template
// assertions.
type Sender interface {
	Send(ctx context.Context, msg Message) error
	// IsConfigured returns true when the underlying transport will
	// actually attempt delivery. NoopClient returns false; SMTPClient
	// returns true. Used by the service-layer startup log to surface
	// a clear "transactional email is disabled" warning when the
	// operator forgot to set SMTP_HOST.
	IsConfigured() bool
}
