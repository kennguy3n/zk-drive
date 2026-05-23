// Package totp implements RFC 6238 Time-Based One-Time-Password (TOTP)
// enrollment, verification, and recovery-code consumption for the
// zk-drive auth layer.
//
// Threat model: a leaked password (DB dump, phishing, reuse from
// another breach) is no longer sufficient to take over an account.
// Possession of the user's authenticator app (the shared secret) is
// required as a second factor.
//
// Storage: shared secrets are encrypted at rest via the same
// internal/crypto.Codec (AES-256-GCM, CREDENTIAL_ENCRYPTION_KEY) that
// protects per-tenant storage credentials. Plaintext exists only in
// memory between Decrypt and the totp.Validate call. Recovery codes
// are bcrypt-hashed via internal/crypto.HashPassword and lookup is
// O(unused codes) — bounded at 10 by design — because bcrypt's random
// salt means we cannot index on the hash.
//
// Replay protection: every successful Verify stamps last_used_at on
// the credential row. The verifier rejects any code whose 30-second
// period begins at or before last_used_at, so a code captured by a
// MITM cannot be replayed during the same period. RFC 6238 §5.2
// mandates this single-use property.
package totp

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Errors surfaced from the Service. Each maps to a distinct HTTP
// status in api/auth/totp.go, so the wire-level distinction matters.
var (
	// ErrNotEnrolled means no row exists in user_totp_credentials
	// for the user. Returned by Verify, Disable, Status when there
	// is nothing to act on.
	ErrNotEnrolled = errors.New("totp: user has no enrolled credential")

	// ErrAlreadyActivated is returned by FinalizeEnrollment when the
	// credential's activated_at is already non-NULL. Surfaces as 409
	// to the client so a buggy frontend doesn't re-issue recovery
	// codes by repeatedly finalizing.
	ErrAlreadyActivated = errors.New("totp: credential is already activated")

	// ErrNotActivated is returned by Verify when the credential row
	// exists but activated_at is NULL — the user began enrollment
	// but never finalized. Distinct from ErrNotEnrolled so login
	// can present the "complete enrollment" affordance instead of
	// the generic "enable 2FA" one.
	ErrNotActivated = errors.New("totp: credential is pending finalize")

	// ErrInvalidCode means the supplied code did not match any
	// period in the configured skew window. Surfaced as 401 with a
	// constant-time generic message ("invalid code") so an attacker
	// cannot distinguish "wrong code" from "code from prior period
	// (replay)" by response timing or text.
	ErrInvalidCode = errors.New("totp: code does not match")

	// ErrCodeReplayed means the supplied code matched but its
	// period start is at or before last_used_at — i.e. a code
	// already accepted in this 30s window. Internal-only; the HTTP
	// layer collapses this onto ErrInvalidCode in the response so
	// the attacker cannot tell whether replay protection or
	// validity rejection fired.
	ErrCodeReplayed = errors.New("totp: code was already used (replay protection)")

	// ErrNoRecoveryCodes means the recovery-code lookup found zero
	// unused codes for the user. Internal-only — the HTTP layer
	// collapses this onto ErrInvalidCode so an attacker cannot
	// distinguish "user has no recovery codes left" from "the code
	// you supplied is wrong".
	ErrNoRecoveryCodes = errors.New("totp: no unused recovery codes")
)

// Credential mirrors the user_totp_credentials row.
type Credential struct {
	UserID          uuid.UUID
	EncryptedSecret string
	ActivatedAt     *time.Time
	LastUsedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IsActivated returns true iff the credential has been finalized.
func (c *Credential) IsActivated() bool {
	return c != nil && c.ActivatedAt != nil
}

// RecoveryCode mirrors the user_totp_recovery_codes row. The
// plaintext code is never stored; CodeHash is bcrypt-hashed.
type RecoveryCode struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	CodeHash  string
	UsedAt    *time.Time
	CreatedAt time.Time
}

// EnrollmentChallenge is what BeginEnrollment returns. The client
// renders the QR code to the user, the user scans it into their
// authenticator app, then submits the resulting 6-digit code back to
// FinalizeEnrollment. The secret is also returned in base32 form so
// the UI can offer a manual-entry fallback (some authenticator apps
// don't support QR-scan on desktop).
type EnrollmentChallenge struct {
	// Secret is the RFC 4648 base32-encoded shared secret. Suitable
	// for manual entry into Google Authenticator, 1Password, etc.
	Secret string
	// OtpauthURI is the canonical otpauth:// URI the authenticator
	// app expects. Encodes the issuer, account label, secret, and
	// algorithm parameters in the RFC 6238 default profile (SHA-1,
	// 6 digits, 30s period).
	OtpauthURI string
	// QRCodePNG is a base64-encoded PNG of OtpauthURI, ~3-5 KB.
	// Returned alongside the URI so a UI without its own QR-code
	// library can still render the scan affordance.
	QRCodePNG string
}

// Status describes a user's enrollment state. Returned by the
// GET /auth/totp/status endpoint and used by the login flow to
// decide whether to issue a session token or an mfa-challenge token.
type Status struct {
	// Enabled is true iff the user has a finalized credential
	// (activated_at IS NOT NULL).
	Enabled bool
	// PendingEnrollment is true iff a credential row exists but is
	// not yet activated. Useful for the UI to resume an interrupted
	// enrollment without forcing the user to regenerate the secret.
	PendingEnrollment bool
	// ActivatedAt is when FinalizeEnrollment first succeeded. NULL
	// while PendingEnrollment is true.
	ActivatedAt *time.Time
	// LastUsedAt is when the most recent successful Verify
	// happened. NULL means no code has ever been accepted (fresh
	// enrollment, no logins yet).
	LastUsedAt *time.Time
	// RecoveryCodesRemaining counts un-used recovery codes. The UI
	// surfaces a warning when this drops to 2 or below so users
	// have time to regenerate before being locked out.
	RecoveryCodesRemaining int
}

// Encryptor is the seam the service uses to encrypt the shared
// secret at rest. Satisfied by *internal/crypto.Codec in production
// and by an identity stub in tests. Mirrors the pattern used by
// internal/storage.ClientFactory.
type Encryptor interface {
	Encrypt(ctx context.Context, plaintext string) (string, error)
	Decrypt(ctx context.Context, ciphertext string) (string, error)
}
