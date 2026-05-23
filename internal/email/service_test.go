package email

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// recordingSender is a Sender that captures every Send call so
// tests can assert end-to-end wiring (template rendering, headers,
// metric outcomes) without standing up a real SMTP listener.
type recordingSender struct {
	mu          sync.Mutex
	calls       []Message
	configured  bool
	sendErr     error
}

func (r *recordingSender) Send(_ context.Context, msg Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, msg)
	if r.sendErr != nil {
		return r.sendErr
	}
	return nil
}

func (r *recordingSender) IsConfigured() bool { return r.configured }

// recordingMetrics implements MetricsRecorder, capturing every
// emit so tests can assert the outcome label is exactly right.
type recordingMetrics struct {
	mu     sync.Mutex
	emits  []emit
}

type emit struct{ template, outcome string }

func (r *recordingMetrics) RecordEmailSent(template, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emits = append(r.emits, emit{template, outcome})
}

func mustService(t *testing.T, sender Sender) (*Service, *recordingMetrics) {
	t.Helper()
	s, err := NewService(ServiceConfig{
		Sender:    sender,
		PublicURL: "https://drive.example.com",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	m := &recordingMetrics{}
	s.WithMetrics(m)
	return s, m
}

// TestSendGuestInvite_HappyPath asserts the service composes the
// accept URL correctly, threads template data into the Message,
// and emits the "ok" metric outcome.
func TestSendGuestInvite_HappyPath(t *testing.T) {
	sender := &recordingSender{configured: true}
	svc, m := mustService(t, sender)
	exp := time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC)
	outcome, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		InviteID:      "INVITEID",
		ExpiresAt:     &exp,
	})
	if err != nil {
		t.Fatalf("SendGuestInvite: %v", err)
	}
	if outcome != OutcomeOK {
		t.Fatalf("outcome = %s, want ok", outcome)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(sender.calls))
	}
	got := sender.calls[0]
	if got.To != "bob@example.com" {
		t.Errorf("To = %q, want bob@example.com", got.To)
	}
	if !strings.Contains(got.Subject, "Acme Co") {
		t.Errorf("Subject = %q, want substring 'Acme Co'", got.Subject)
	}
	if !strings.Contains(got.TextBody, "https://drive.example.com/invites/INVITEID") {
		t.Errorf("TextBody missing accept URL: %s", got.TextBody)
	}
	if got.Headers["Auto-Submitted"] != "auto-generated" {
		t.Errorf("Auto-Submitted = %q, want auto-generated", got.Headers["Auto-Submitted"])
	}
	if len(m.emits) != 1 || m.emits[0].template != "guest_invite" || m.emits[0].outcome != "ok" {
		t.Errorf("metric emits = %+v, want one {guest_invite, ok}", m.emits)
	}
}

// TestSendGuestInvite_DisabledSenderEmitsDisabledMetric pins the
// branchless-no-op contract: when SMTP isn't configured the call
// returns (OutcomeDisabled, nil) so the caller's hot path stays
// simple AND short-circuits BEFORE template render / Message
// construction. The latter property matters when the wired Sender
// is a NoopClient paired with a placeholder publicURL ("http://invalid.local"
// in buildEmailService's PUBLIC_URL-missing arm) — composing the
// accept URL there is pure waste, and worse, a future regression
// that exposes the rendered Message somewhere visible would leak
// the malformed URL.
func TestSendGuestInvite_DisabledSenderEmitsDisabledMetric(t *testing.T) {
	sender := &recordingSender{configured: false, sendErr: ErrNotConfigured}
	svc, m := mustService(t, sender)
	outcome, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		InviteID:      "INVITEID",
	})
	if err != nil {
		t.Fatalf("SendGuestInvite returned err for disabled sender: %v", err)
	}
	if outcome != OutcomeDisabled {
		t.Fatalf("outcome = %s, want disabled", outcome)
	}
	if len(m.emits) != 1 || m.emits[0].outcome != "disabled" {
		t.Errorf("metric outcome = %+v, want disabled", m.emits)
	}
	// Critical: assert the disabled short-circuit fired BEFORE
	// any template/render work or Sender.Send call. The
	// recordingSender records every Send invocation, so zero calls
	// here means we never reached the render+send block.
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 0 {
		t.Errorf("disabled sender saw %d Send call(s); expected 0 — sendGuestInvite should short-circuit BEFORE render+Send", len(sender.calls))
	}
}

// TestSendGuestInvite_SMTPErrorIsClassifiedAndReturned asserts a
// transport-level failure surfaces as an error AND a smtp_error
// metric. The caller is expected to log + continue.
func TestSendGuestInvite_SMTPErrorIsClassifiedAndReturned(t *testing.T) {
	sender := &recordingSender{configured: true, sendErr: errors.New("dial failed")}
	svc, m := mustService(t, sender)
	outcome, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		InviteID:      "INVITEID",
	})
	if err == nil {
		t.Fatalf("expected error from SMTP failure")
	}
	if outcome != OutcomeSMTPError {
		t.Fatalf("outcome = %s, want smtp_error", outcome)
	}
	if len(m.emits) != 1 || m.emits[0].outcome != "smtp_error" {
		t.Errorf("metric outcome = %+v, want smtp_error", m.emits)
	}
}

// TestSendGuestInvite_AddressInvalidIsClassified asserts the
// end-to-end classification path: a sender that returns an
// ErrInvalidAddress-wrapped error must produce
// OutcomeAddressInvalid (and the corresponding metric label),
// not OutcomeSMTPError. The real SMTPClient.Send wraps
// mail.ParseAddress failures with ErrInvalidAddress, so this
// test pins the contract that the classifier observes.
func TestSendGuestInvite_AddressInvalidIsClassified(t *testing.T) {
	sender := &recordingSender{configured: true, sendErr: fmt.Errorf("%w: %v", ErrInvalidAddress, errors.New("mail: missing '@'"))}
	svc, m := mustService(t, sender)
	outcome, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		InviteID:      "INVITEID",
	})
	if err == nil {
		t.Fatalf("expected error for invalid address")
	}
	if outcome != OutcomeAddressInvalid {
		t.Fatalf("outcome = %s, want address_invalid", outcome)
	}
	if len(m.emits) != 1 || m.emits[0].outcome != "address_invalid" {
		t.Errorf("metric outcome = %+v, want address_invalid", m.emits)
	}
}

// TestSendGuestInvite_TemplateErrorReturnsBeforeSend asserts a
// render failure (missing required field) is classified as
// template_error AND the sender is NOT called \u2014 keeping the
// metric semantics honest.
func TestSendGuestInvite_TemplateErrorReturnsBeforeSend(t *testing.T) {
	sender := &recordingSender{configured: true}
	svc, m := mustService(t, sender)
	outcome, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:    "bob@example.com",
		InviteID: "X",
	})
	if err == nil {
		t.Fatalf("expected error for missing template fields")
	}
	if outcome != OutcomeTemplateError {
		t.Fatalf("outcome = %s, want template_error", outcome)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("sender called %d times; should be 0 when template fails", len(sender.calls))
	}
	if len(m.emits) != 1 || m.emits[0].outcome != "template_error" {
		t.Errorf("metric outcome = %+v, want template_error", m.emits)
	}
}

// TestNewService_RejectsEmptySender enforces the NewService
// contract \u2014 a programming error at boot fails fast, not at
// first Send.
func TestNewService_RejectsEmptySender(t *testing.T) {
	_, err := NewService(ServiceConfig{PublicURL: "https://drive.example.com"})
	if err == nil {
		t.Fatalf("expected error for nil Sender")
	}
}

// TestNewService_RejectsEmptyPublicURL enforces the NewService
// contract \u2014 forgetting PUBLIC_URL means every invite link is
// broken; better to fail at boot than to ship corrupt URLs.
func TestNewService_RejectsEmptyPublicURL(t *testing.T) {
	_, err := NewService(ServiceConfig{Sender: &recordingSender{}})
	if err == nil {
		t.Fatalf("expected error for empty PublicURL")
	}
}

// TestNewService_TrimsTrailingSlash keeps the URL composer
// branchless \u2014 either form is accepted, the composed URL never
// contains a double slash.
func TestNewService_TrimsTrailingSlash(t *testing.T) {
	sender := &recordingSender{configured: true}
	svc, err := NewService(ServiceConfig{Sender: sender, PublicURL: "https://drive.example.com/"})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme Co",
		FolderName:    "Q4 Roadmap",
		Role:          "editor",
		InviteID:      "INVITEID",
	}); err != nil {
		t.Fatalf("SendGuestInvite: %v", err)
	}
	got := sender.calls[0].TextBody
	if strings.Contains(got, "drive.example.com//invites") {
		t.Fatalf("composed URL has double slash: %s", got)
	}
	if !strings.Contains(got, "drive.example.com/invites/INVITEID") {
		t.Fatalf("composed URL not present: %s", got)
	}
}

// TestNewService_TrimsMultipleTrailingSlashes pins the multi-slash
// guard: an operator who sets PUBLIC_URL="https://drive.example.com///"
// (typo, copy-paste from a config that ended in a path separator,
// or template rendering glitch) MUST NOT produce a triple-slash
// accept URL. The single-trim implementation would leave "x.com//"
// which composes to "x.com//invites/abc" — most browsers/clients
// normalize but some routers/CDNs treat that as a different path.
// strings.TrimRight handles every trailing-slash count in one pass.
func TestNewService_TrimsMultipleTrailingSlashes(t *testing.T) {
	for _, suffix := range []string{"//", "///", "////"} {
		t.Run(suffix, func(t *testing.T) {
			sender := &recordingSender{configured: true}
			svc, err := NewService(ServiceConfig{Sender: sender, PublicURL: "https://drive.example.com" + suffix})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if _, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
				Email:         "bob@example.com",
				InviterName:   "Alice",
				WorkspaceName: "Acme Co",
				FolderName:    "Q4 Roadmap",
				Role:          "editor",
				InviteID:      "INVITEID",
			}); err != nil {
				t.Fatalf("SendGuestInvite: %v", err)
			}
			got := sender.calls[0].TextBody
			if strings.Contains(got, "drive.example.com//") {
				t.Fatalf("composed URL retained a double slash for suffix %q: %s", suffix, got)
			}
			if !strings.Contains(got, "drive.example.com/invites/INVITEID") {
				t.Fatalf("composed URL not present for suffix %q: %s", suffix, got)
			}
		})
	}
}

// TestLogStartup_DisabledReasonSurfaced pins the operator-facing
// contract: when ServiceConfig.DisabledReason is set (e.g. because
// PUBLIC_URL is missing even though SMTP_* are configured),
// LogStartup must surface that reason instead of telling the
// operator to "set SMTP_HOST/SMTP_PORT/SMTP_FROM_ADDRESS" — which
// would be actively misleading.
func TestLogStartup_DisabledReasonSurfaced(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc, err := NewService(ServiceConfig{
		Sender:         &recordingSender{configured: false},
		PublicURL:      "http://invalid.local",
		DisabledReason: "PUBLIC_URL is not set — composed accept-invite links would be malformed",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := logging.WithContext(context.Background(), log)
	svc.LogStartup(ctx)
	out := buf.String()
	if !strings.Contains(out, "PUBLIC_URL is not set") {
		t.Errorf("LogStartup did not surface DisabledReason; got %q", out)
	}
	if strings.Contains(out, "SMTP_HOST/SMTP_PORT") {
		t.Errorf("LogStartup misleadingly told operator to set SMTP_* even though DisabledReason was set; got %q", out)
	}
}

// TestLogStartup_DefaultDisabledMessage pins the no-reason path:
// when DisabledReason is empty and the Sender is NoopClient
// (i.e. SMTP_HOST is the missing config), the warn message
// instructs the operator to set SMTP_*.
func TestLogStartup_DefaultDisabledMessage(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc, err := NewService(ServiceConfig{
		Sender:    &recordingSender{configured: false},
		PublicURL: "https://drive.example.com",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := logging.WithContext(context.Background(), log)
	svc.LogStartup(ctx)
	out := buf.String()
	if !strings.Contains(out, "SMTP_HOST/SMTP_PORT") {
		t.Errorf("LogStartup default-disabled message missing SMTP_* hint; got %q", out)
	}
}

// TestLogStartup_EnabledLogsInfo pins the happy path: a configured
// Sender produces an info line (not a warning) with the public URL.
func TestLogStartup_EnabledLogsInfo(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc, err := NewService(ServiceConfig{
		Sender:    &recordingSender{configured: true},
		PublicURL: "https://drive.example.com",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := logging.WithContext(context.Background(), log)
	svc.LogStartup(ctx)
	out := buf.String()
	if !strings.Contains(out, "transactional email enabled") {
		t.Errorf("LogStartup enabled-path missing expected message; got %q", out)
	}
	if !strings.Contains(out, "https://drive.example.com") {
		t.Errorf("LogStartup enabled-path missing public_url; got %q", out)
	}
}

// TestMaskEmail rounds out the small surface: PII redaction
// happens before slog so per-send observability lines never leak a
// full address. Lock the format so future changes don't regress.
func TestMaskEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"alice@example.com", "a***@example.com"},
		{"a@example.com", "a***@example.com"},
		{"", "***"},
		{"no-at-sign", "***"},
		{"@bad", "***"},
	}
	for _, c := range cases {
		if got := maskEmail(c.in); got != c.want {
			t.Errorf("maskEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
