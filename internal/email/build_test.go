package email

import (
	"context"
	"strings"
	"testing"
)

// TestBuildFromOperatorConfig_PublicURLMissing pins the first
// graceful-degrade case: PUBLIC_URL is the most critical missing
// var because composed accept-invite links would be malformed even
// if SMTP is fully configured. The Service must be wired to a
// NoopClient and carry a DisabledReason that names PUBLIC_URL
// specifically (a generic "set SMTP_*" hint would actively mislead
// an operator who set every SMTP_* var).
func TestBuildFromOperatorConfig_PublicURLMissing(t *testing.T) {
	svc, err := BuildFromOperatorConfig(OperatorConfig{
		// PublicURL intentionally empty
		SMTPHost:        "smtp.example.com",
		SMTPPort:        587,
		SMTPFromAddress: "drive@example.com",
		SMTPTLSMode:     "starttls",
	})
	if err != nil {
		t.Fatalf("BuildFromOperatorConfig returned err: %v", err)
	}
	if svc == nil {
		t.Fatalf("BuildFromOperatorConfig returned nil service")
	}
	// Sender must be wired to Noop so Send returns ErrNotConfigured.
	if _, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme",
		FolderName:    "Q4",
		Role:          "editor",
		InviteID:      "INVITEID",
	}); err != nil {
		t.Fatalf("SendGuestInvite under disabled config returned err: %v", err)
	}
	// Inspect the disabled reason via LogStartup's behavior: the
	// service exposes its config via the public methods, but the
	// surest test is to drive LogStartup and assert the line
	// names PUBLIC_URL.
	if !strings.Contains(svc.disabledReason, "PUBLIC_URL") {
		t.Errorf("disabled reason does not name PUBLIC_URL: %q", svc.disabledReason)
	}
}

// TestBuildFromOperatorConfig_SMTPHostMissing pins the second
// graceful-degrade case: the common dev / metadata-only path
// where the operator hasn't configured an SMTP relay. No
// DisabledReason here because the "set SMTP_HOST" hint in
// LogStartup's default-disabled-message arm is accurate.
func TestBuildFromOperatorConfig_SMTPHostMissing(t *testing.T) {
	svc, err := BuildFromOperatorConfig(OperatorConfig{
		PublicURL:       "https://drive.example.com",
		SMTPFromAddress: "drive@example.com",
		// SMTPHost intentionally empty
	})
	if err != nil {
		t.Fatalf("BuildFromOperatorConfig returned err: %v", err)
	}
	if svc == nil {
		t.Fatalf("BuildFromOperatorConfig returned nil service")
	}
	if svc.disabledReason != "" {
		t.Errorf("SMTP_HOST-missing case should NOT carry a DisabledReason (LogStartup's default hint covers it); got %q", svc.disabledReason)
	}
	// Confirm wired to Noop.
	if _, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme",
		FolderName:    "Q4",
		Role:          "editor",
		InviteID:      "INVITEID",
	}); err != nil {
		t.Fatalf("SendGuestInvite under disabled config returned err: %v", err)
	}
}

// TestBuildFromOperatorConfig_SMTPFromAddressMissing pins the
// critical contract: SMTP_HOST is set but SMTP_FROM_ADDRESS is
// missing. NewSMTPClient hard-errors on empty FromAddress, which
// would crash startup. The README at "Transactional email"
// explicitly promises "omit any one required env var to leave
// email disabled — the server boots cleanly in disabled mode" —
// without this branch, the contract is violated.
//
// Regression test for BUG_0001 from the second Devin Review pass
// on PR #66.
func TestBuildFromOperatorConfig_SMTPFromAddressMissing(t *testing.T) {
	svc, err := BuildFromOperatorConfig(OperatorConfig{
		PublicURL:   "https://drive.example.com",
		SMTPHost:    "smtp.example.com",
		SMTPPort:    587,
		SMTPTLSMode: "starttls",
		// SMTPFromAddress intentionally empty
	})
	if err != nil {
		t.Fatalf("BuildFromOperatorConfig must NOT return an error when SMTP_FROM_ADDRESS is missing; the README contract requires graceful degradation. Got err: %v", err)
	}
	if svc == nil {
		t.Fatalf("BuildFromOperatorConfig returned nil service for SMTP_FROM_ADDRESS-missing case")
	}
	if !strings.Contains(svc.disabledReason, "SMTP_FROM_ADDRESS") {
		t.Errorf("disabled reason must name SMTP_FROM_ADDRESS so operator sees the specific missing var (not a generic 'set SMTP_HOST' hint which would be misleading when they already set SMTP_HOST); got %q", svc.disabledReason)
	}
	// Wired to Noop — Send returns disabled, not error.
	outcome, err := svc.SendGuestInvite(context.Background(), SendGuestInviteInput{
		Email:         "bob@example.com",
		InviterName:   "Alice",
		WorkspaceName: "Acme",
		FolderName:    "Q4",
		Role:          "editor",
		InviteID:      "INVITEID",
	})
	if err != nil {
		t.Fatalf("SendGuestInvite under SMTP_FROM_ADDRESS-missing config returned err: %v", err)
	}
	if outcome != OutcomeDisabled {
		t.Errorf("outcome = %s, want disabled (a NoopClient was wired so Send must classify as disabled)", outcome)
	}
}

// TestBuildFromOperatorConfig_FullyConfigured exercises the
// happy path — all required vars set, NewSMTPClient is called
// for real. This pins that the wiring forwards the SMTPConfig
// fields correctly. We can't actually open a connection here
// without a real relay, but constructing the SMTPClient itself
// must succeed.
func TestBuildFromOperatorConfig_FullyConfigured(t *testing.T) {
	svc, err := BuildFromOperatorConfig(OperatorConfig{
		PublicURL:       "https://drive.example.com",
		SMTPHost:        "smtp.example.com",
		SMTPPort:        587,
		SMTPUsername:    "drive@example.com",
		SMTPPassword:    "secret",
		SMTPFromAddress: "drive@example.com",
		SMTPFromName:    "ZK Drive",
		SMTPTLSMode:     "starttls",
	})
	if err != nil {
		t.Fatalf("BuildFromOperatorConfig fully-configured returned err: %v", err)
	}
	if svc == nil {
		t.Fatalf("BuildFromOperatorConfig fully-configured returned nil service")
	}
	if svc.disabledReason != "" {
		t.Errorf("fully-configured service must NOT carry a DisabledReason; got %q", svc.disabledReason)
	}
}
