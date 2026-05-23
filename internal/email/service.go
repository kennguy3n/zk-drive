package email

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// SendOutcome is the metric-friendly status returned by Service
// methods so the call-site can record a bounded-cardinality label.
// Kept as a string type for direct use as a Prometheus label.
type SendOutcome string

const (
	// OutcomeOK indicates the message was handed off to the SMTP
	// relay (250 OK on DATA). Note: this does NOT guarantee
	// inbox delivery — providers reject or bounce later via the
	// envelope-sender (out-of-band).
	OutcomeOK SendOutcome = "ok"
	// OutcomeTemplateError indicates a render failure (missing
	// data field, malformed template body). No SMTP attempt
	// happened. This is always a server bug — the call-site
	// should surface it at slog.Error level.
	OutcomeTemplateError SendOutcome = "template_error"
	// OutcomeSMTPError indicates the SMTP transport refused the
	// message (connection refused, auth failure, RCPT TO
	// rejected, etc.). Transient — callers SHOULD log at
	// slog.Warn and continue.
	OutcomeSMTPError SendOutcome = "smtp_error"
	// OutcomeDisabled indicates the service is wired to a
	// NoopClient (no SMTP_HOST configured). Expected in dev /
	// metadata-only modes; callers should log at slog.Debug.
	OutcomeDisabled SendOutcome = "disabled"
	// OutcomeAddressInvalid indicates the recipient address
	// failed mail.ParseAddress before any transport attempt.
	// Surfaces a bad user input; callers should log at slog.Warn.
	OutcomeAddressInvalid SendOutcome = "address_invalid"
)

// MetricsRecorder is the Prometheus-shaped surface the email
// service uses to emit per-send observability. Implemented by
// internal/metrics.Metrics — kept as an interface here so the
// email package does not depend on the metrics package directly
// (which would cycle through cmd/server's wiring).
type MetricsRecorder interface {
	RecordEmailSent(template string, outcome string)
}

// Service is the call-site-facing entry point. It owns:
//
//   - the Sender (real SMTP or Noop),
//   - the template registry (currently just the guest-invite pair),
//   - the metrics + audit surfaces.
//
// Methods are typed per-event (SendGuestInvite, ...) so callers do
// not need to construct Message values directly. This mirrors the
// notification.Service shape and keeps subject lines / template
// IDs out of the call-site.
type Service struct {
	sender    Sender
	publicURL string
	metrics   MetricsRecorder
}

// ServiceConfig is the dependency bundle for NewService. PublicURL
// is the canonical externally-reachable base URL of the frontend
// (e.g. "https://drive.example.com") — used to compose
// accept-invite links. Trailing slashes are normalised.
type ServiceConfig struct {
	Sender    Sender
	PublicURL string
}

// NewService constructs a Service. Both Sender and PublicURL are
// required; metrics are wired separately via WithMetrics to match
// the composable-setter pattern used by the drive handler.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Sender == nil {
		return nil, errors.New("email: Sender is required")
	}
	if cfg.PublicURL == "" {
		return nil, errors.New("email: PublicURL is required to compose accept-invite links")
	}
	publicURL := trimTrailingSlash(cfg.PublicURL)
	return &Service{
		sender:    cfg.Sender,
		publicURL: publicURL,
	}, nil
}

// WithMetrics attaches a metrics recorder so every Send call emits
// the zkdrive_email_sent_total counter. A nil recorder makes
// Record* a no-op (kept that way so test wiring stays cheap).
func (s *Service) WithMetrics(m MetricsRecorder) *Service {
	s.metrics = m
	return s
}

// IsConfigured reports whether the underlying Sender will attempt
// delivery. False when running on NoopClient.
func (s *Service) IsConfigured() bool { return s.sender.IsConfigured() }

// LogStartup writes a single slog.Info / slog.Warn line at boot so
// operators can see the transactional-email mode without grepping
// for SMTP_HOST. Called by cmd/server.main once after Load().
func (s *Service) LogStartup(ctx context.Context) {
	log := logging.FromContext(ctx)
	if s.IsConfigured() {
		log.Info("transactional email enabled", "public_url", s.publicURL)
	} else {
		log.Warn("transactional email DISABLED — set SMTP_HOST/SMTP_PORT/SMTP_FROM_ADDRESS to enable guest-invite delivery (currently best-effort no-op)")
	}
}

// SendGuestInviteInput is the call-site-facing payload for the
// guest-invite email. Email is the recipient; the rest is template
// data. ExpiresAt is optional.
type SendGuestInviteInput struct {
	Email         string
	RecipientName string // optional, used to format "Alice <alice@example.com>"
	InviterName   string
	WorkspaceName string
	FolderName    string
	Role          string
	InviteID      string // used to compose AcceptURL = PublicURL + "/invites/" + InviteID
	ExpiresAt     *time.Time
}

// SendGuestInvite renders + sends the guest-invite email. Returns
// the outcome so the caller can record it on audit / log lines.
// The error is non-nil only for OutcomeSMTPError, OutcomeTemplateError,
// and OutcomeAddressInvalid — OutcomeOK and OutcomeDisabled both
// return nil so the most common "happy path + dev path" hot loops
// stay branchless at the call-site.
func (s *Service) SendGuestInvite(ctx context.Context, in SendGuestInviteInput) (SendOutcome, error) {
	outcome, err := s.sendGuestInvite(ctx, in)
	s.record("guest_invite", outcome)
	logSendOutcome(ctx, "guest_invite", in.Email, outcome, err)
	return outcome, err
}

func (s *Service) sendGuestInvite(ctx context.Context, in SendGuestInviteInput) (SendOutcome, error) {
	data := GuestInviteData{
		InviterName:   in.InviterName,
		WorkspaceName: in.WorkspaceName,
		FolderName:    in.FolderName,
		Role:          in.Role,
		Email:         in.Email,
		AcceptURL:     s.publicURL + "/invites/" + in.InviteID,
	}
	if in.ExpiresAt != nil {
		data.ExpiresAt = in.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	rendered, err := renderGuestInvite(data)
	if err != nil {
		return OutcomeTemplateError, err
	}
	msg := Message{
		To:            in.Email,
		RecipientName: in.RecipientName,
		Subject:       "You've been invited to " + in.WorkspaceName + " on ZK Drive",
		TextBody:      rendered.Text,
		HTMLBody:      rendered.HTML,
		Headers: map[string]string{
			// RFC 3834 — flag templated system mail so user mailers
			// can suppress auto-replies / vacation responders.
			"Auto-Submitted": "auto-generated",
		},
		TemplateName: "guest_invite",
	}
	if err := s.sender.Send(ctx, msg); err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return OutcomeDisabled, nil
		}
		return classifySendError(err), err
	}
	return OutcomeOK, nil
}

func (s *Service) record(template string, outcome SendOutcome) {
	if s.metrics == nil {
		return
	}
	s.metrics.RecordEmailSent(template, string(outcome))
}

// classifySendError maps the bag of SMTPClient errors onto a
// metric-friendly outcome label. Today only address-invalid and
// general SMTP errors are distinguished — additional buckets
// (e.g. greylisted) can be added without changing the call sites.
func classifySendError(err error) SendOutcome {
	if err == nil {
		return OutcomeOK
	}
	switch {
	case errors.Is(err, ErrInvalidAddress):
		return OutcomeAddressInvalid
	default:
		return OutcomeSMTPError
	}
}

// ErrInvalidAddress is returned by the SMTP layer when the
// recipient address fails RFC 5322 parsing. Exposed for callers
// that want to differentiate "fix the user input" from "the relay
// is down".
var ErrInvalidAddress = errors.New("email: recipient address is invalid")

func logSendOutcome(ctx context.Context, template, to string, outcome SendOutcome, err error) {
	log := logging.FromContext(ctx)
	attrs := []any{
		"template", template,
		"to", maskEmail(to),
		"outcome", string(outcome),
	}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	switch outcome {
	case OutcomeOK:
		log.Info("email sent", attrs...)
	case OutcomeDisabled:
		log.Debug("email skipped: transactional email disabled", attrs...)
	case OutcomeTemplateError:
		log.Error("email render failed", attrs...)
	case OutcomeSMTPError, OutcomeAddressInvalid:
		log.Warn("email send failed", attrs...)
	default:
		log.Warn("email send unknown outcome", attrs...)
	}
}

// maskEmail collapses the local-part to first char + asterisks so
// the operator log doesn't echo a full PII address on every send.
// Standard practice for transactional-email observability.
func maskEmail(addr string) string {
	at := -1
	for i, r := range addr {
		if r == '@' {
			at = i
			break
		}
	}
	if at <= 0 {
		return "***"
	}
	return string(addr[0]) + "***" + addr[at:]
}

// trimTrailingSlash removes a single trailing "/" if present, so
// callers can pass either "https://drive.example.com" or
// "https://drive.example.com/" without composing a double-slash
// URL like ".../invites//abc".
func trimTrailingSlash(s string) string {
	if len(s) > 0 && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}

// SlogLevelForOutcome is exposed so tests can assert which level
// a given outcome would log at, without depending on a captured
// logger. Not used in production code outside logSendOutcome.
func SlogLevelForOutcome(o SendOutcome) slog.Level {
	switch o {
	case OutcomeOK:
		return slog.LevelInfo
	case OutcomeDisabled:
		return slog.LevelDebug
	case OutcomeTemplateError:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}
