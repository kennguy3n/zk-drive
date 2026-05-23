package totp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	appcrypto "github.com/kennguy3n/zk-drive/internal/crypto"
)

// SecretBytes is the size of the shared secret in bytes.
// RFC 4226 §4 recommends at least 16 bytes; we use 20 (160 bits)
// which matches RFC 6238's HOTP-SHA1 default profile and is what
// Google Authenticator / 1Password / Authy default to.
const SecretBytes = 20

// PeriodSeconds is the TOTP step size. 30 seconds is the RFC 6238
// default and the only value broadly supported by authenticator
// apps.
const PeriodSeconds = 30

// Digits is the length of the generated 6-digit code. RFC 6238
// allows 6/7/8; 6 is universal and what every consumer-grade app
// renders.
const Digits = otp.DigitsSix

// SkewPeriods is the number of 30-second windows on either side of
// "now" that we accept. ±1 = current, previous, and next. RFC 6238
// §5.2 recommends a small skew to tolerate clock drift between the
// server and the user's phone; anything wider weakens the
// possession factor without proportional UX benefit.
const SkewPeriods = 1

// RecoveryCodeCount is the number of recovery codes issued at
// FinalizeEnrollment. 10 is the industry standard (GitHub, Google,
// AWS). Sized to balance "user has enough to survive multiple
// device losses without re-enrolling" against "an attacker who gets
// one shouldn't have many more to try".
const RecoveryCodeCount = 10

// RecoveryCodeGroups / RecoveryCodeGroupSize define the displayed
// format of each code: 5 dash-separated groups of 2 characters,
// e.g. "xb-4q-9z-pm-tk". 10 characters of base32 alphabet =
// 50 bits of entropy per code, which is well above the brute-force
// floor (an attacker would need to make 2^50 / 10 ~= 10^14 guesses
// against a single user's pool to find one).
const (
	RecoveryCodeGroups    = 5
	RecoveryCodeGroupSize = 2
)

// recoveryAlphabet is the user-facing display alphabet for recovery
// codes. We avoid '0'/'O' and '1'/'I'/'l' to reduce transcription
// errors when users copy codes from a printed page.
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// Service is the public API for TOTP enrollment, verification, and
// recovery-code consumption. All operations are user-scoped; the
// caller (auth handlers) is responsible for enforcing that the
// userID belongs to the authenticated principal.
type Service struct {
	repo   Repository
	codec  Encryptor
	clock  func() time.Time
	issuer string
}

// NewService constructs a Service. issuer is the human-readable
// label rendered by authenticator apps (e.g. "zk-drive"). codec
// encrypts/decrypts the shared secret at rest — pass the same
// *internal/crypto.Codec instance used for storage credentials.
func NewService(repo Repository, codec Encryptor, issuer string) *Service {
	return &Service{
		repo:   repo,
		codec:  codec,
		clock:  func() time.Time { return time.Now().UTC() },
		issuer: issuer,
	}
}

// WithClock overrides the time source. Test-only: the clock is
// otherwise a thin wrapper around time.Now that always returns UTC
// so daylight-savings transitions cannot cause a phantom
// "code-from-the-future" failure on the server.
func (s *Service) WithClock(c func() time.Time) *Service {
	s.clock = c
	return s
}

// BeginEnrollment generates a fresh shared secret for the user,
// stores it in the pending state (activated_at = NULL), and returns
// the otpauth URI + QR PNG for the client to display.
//
// Semantics:
//   - No existing row: create one in pending state.
//   - Existing pending row: overwrite in place (the user clicked
//     "begin enrollment" twice; they want a fresh secret).
//   - Existing ACTIVATED row: refuse with ErrAlreadyActivated. The
//     caller must Disable first.
//
// Refusing to overwrite an activated row is essential to prevent a
// "user clicks re-enroll, abandons before Finalize, gets locked
// out" failure mode: with overwrite-in-place semantics, the active
// secret would be destroyed the moment BeginEnrollment runs, but
// the new secret is not usable until FinalizeEnrollment commits
// the matching code. Industry-standard 2FA flows (GitHub, Google,
// AWS) all require an explicit Disable before re-enrollment for
// exactly this reason.
//
// Account label is "<issuer>:<email>" by convention; the email is
// what the authenticator app shows next to the entry.
func (s *Service) BeginEnrollment(ctx context.Context, userID uuid.UUID, accountLabel string) (*EnrollmentChallenge, error) {
	if accountLabel == "" {
		return nil, errors.New("totp: accountLabel is required")
	}

	existing, err := s.repo.GetCredential(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotEnrolled) {
		return nil, fmt.Errorf("probe existing credential: %w", err)
	}
	if existing != nil && existing.IsActivated() {
		return nil, ErrAlreadyActivated
	}

	secretBytes := make([]byte, SecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("read random secret: %w", err)
	}
	// base32 (RFC 4648, no padding) is the encoding pquerna/otp
	// expects for Key.Secret(). It's also what authenticator apps
	// display when users tap "enter setup key" instead of scanning
	// the QR.
	secret := strings.TrimRight(base32.StdEncoding.EncodeToString(secretBytes), "=")

	// Build the otpauth URI explicitly so we control every field
	// (issuer, label, algorithm, digits, period). pquerna/otp's
	// totp.Generate would do this for us, but Generate also
	// generates the secret internally — we want to control that
	// step because we just generated it from crypto/rand above.
	key, err := otp.NewKeyFromURL(buildOtpauthURI(s.issuer, accountLabel, secret))
	if err != nil {
		return nil, fmt.Errorf("build otpauth key: %w", err)
	}

	encrypted, err := s.codec.Encrypt(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("encrypt totp secret: %w", err)
	}

	if err := s.repo.UpsertCredential(ctx, &Credential{
		UserID:          userID,
		EncryptedSecret: encrypted,
		ActivatedAt:     nil,
		LastUsedAt:      nil,
	}); err != nil {
		return nil, fmt.Errorf("upsert pending credential: %w", err)
	}

	pngB64, err := renderQRCodePNG(key)
	if err != nil {
		return nil, fmt.Errorf("render qr code: %w", err)
	}

	return &EnrollmentChallenge{
		Secret:     secret,
		OtpauthURI: key.URL(),
		QRCodePNG:  pngB64,
	}, nil
}

// FinalizeEnrollment verifies the user-submitted code against the
// pending secret and, on success, activates the credential and
// issues a fresh set of plaintext recovery codes. The returned
// codes are shown to the user EXACTLY ONCE — they are not
// persisted in plaintext anywhere.
//
// Re-enrollment semantics: any pre-existing recovery codes for the
// user (used or unused) are wiped before the new batch is inserted.
// This is essential because the prior codes were tied to a secret
// the user may have lost control of; keeping them valid would mean
// a re-enrolled account could still be reached with the old codes.
//
// Returns ErrAlreadyActivated if the credential is already finalized,
// ErrNotEnrolled if no pending row exists, and ErrInvalidCode if
// the supplied code does not match the pending secret.
func (s *Service) FinalizeEnrollment(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	cred, err := s.repo.GetCredential(ctx, userID)
	if err != nil {
		return nil, err
	}
	if cred.IsActivated() {
		return nil, ErrAlreadyActivated
	}

	secret, err := s.codec.Decrypt(ctx, cred.EncryptedSecret)
	if err != nil {
		return nil, fmt.Errorf("decrypt totp secret: %w", err)
	}

	if !validate(code, secret, s.clock(), SkewPeriods) {
		return nil, ErrInvalidCode
	}

	plaintextCodes, hashes, err := generateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, fmt.Errorf("generate recovery codes: %w", err)
	}

	if err := s.repo.FinalizeEnrollment(ctx, userID, s.clock(), hashes); err != nil {
		return nil, err
	}
	return plaintextCodes, nil
}

// Verify accepts a 6-digit TOTP code against the activated
// credential. On success it stamps last_used_at to prevent replay
// within the same period.
//
// Returns ErrNotEnrolled if no credential row exists,
// ErrNotActivated if a pending row exists, ErrInvalidCode for any
// mismatch (including replays — the caller gets one error code so
// an attacker cannot distinguish "wrong code" from "right code,
// already used"), and a wrapped error for unexpected DB / decrypt
// failures.
func (s *Service) Verify(ctx context.Context, userID uuid.UUID, code string) error {
	cred, err := s.repo.GetCredential(ctx, userID)
	if err != nil {
		return err
	}
	if !cred.IsActivated() {
		return ErrNotActivated
	}

	secret, err := s.codec.Decrypt(ctx, cred.EncryptedSecret)
	if err != nil {
		return fmt.Errorf("decrypt totp secret: %w", err)
	}

	now := s.clock()
	if !validate(code, secret, now, SkewPeriods) {
		return ErrInvalidCode
	}

	// Replay protection: the period of the code we just accepted
	// must be strictly after last_used_at. We don't know which
	// period within the ±skew window matched, but the worst case
	// is "the user's clock is 30s behind the server" — in which
	// case the matched period is now - 30s. Pinning last_used_at
	// to "now" rejects every code whose period is <= now's period
	// on the next call, including the one we just accepted.
	if cred.LastUsedAt != nil && !now.After(*cred.LastUsedAt) {
		return ErrCodeReplayed
	}

	if err := s.repo.UpdateLastUsed(ctx, userID, now); err != nil {
		return fmt.Errorf("update last_used_at: %w", err)
	}
	return nil
}

// ConsumeRecoveryCode accepts a recovery code (in the user-visible
// dash-separated form) and burns it.  On success the next verify
// call will require a fresh TOTP code or a different recovery code.
//
// Because bcrypt uses a random salt, the hash for the same code
// differs every time it's generated. We therefore cannot index by
// hash and must compare against every un-used row for the user.
// The partial index keeps that list bounded at <=10 by design.
//
// Returns ErrInvalidCode for any mismatch (or zero unused codes),
// collapsing all failure paths so an attacker cannot distinguish
// "wrong code" from "user has no codes left".
func (s *Service) ConsumeRecoveryCode(ctx context.Context, userID uuid.UUID, code string) error {
	normalized := normalizeRecoveryCode(code)
	if !isWellFormedRecoveryCode(normalized) {
		return ErrInvalidCode
	}

	codes, err := s.repo.ListUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return fmt.Errorf("list unused recovery codes: %w", err)
	}
	if len(codes) == 0 {
		// Collapsed onto ErrInvalidCode so the response timing
		// reveals "wrong" vs "exhausted" only via the (bounded)
		// bcrypt iteration count — which is constant given the
		// partial index ceiling of 10 rows.
		return ErrInvalidCode
	}

	for _, rc := range codes {
		if bcrypt.CompareHashAndPassword([]byte(rc.CodeHash), []byte(normalized)) != nil {
			continue
		}
		if err := s.repo.MarkRecoveryCodeUsed(ctx, rc.ID, s.clock()); err != nil {
			if errors.Is(err, ErrCodeReplayed) {
				// Race lost — another verify burned this row
				// between the list and the update. Treat as
				// invalid for the caller; the simultaneous use
				// is already audit-logged on the winning path.
				return ErrInvalidCode
			}
			return fmt.Errorf("mark recovery code used: %w", err)
		}
		return nil
	}
	return ErrInvalidCode
}

// Disable removes the user's credential and all recovery codes.
// Caller is responsible for re-verifying the password before
// calling this — Disable is a hard, immediate action.
func (s *Service) Disable(ctx context.Context, userID uuid.UUID) error {
	return s.repo.DeleteCredential(ctx, userID)
}

// Status returns the user's enrollment state without exposing the
// secret.
func (s *Service) Status(ctx context.Context, userID uuid.UUID) (*Status, error) {
	cred, err := s.repo.GetCredential(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotEnrolled) {
			return &Status{}, nil
		}
		return nil, err
	}
	count, err := s.repo.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &Status{
		Enabled:                cred.IsActivated(),
		PendingEnrollment:      !cred.IsActivated(),
		ActivatedAt:            cred.ActivatedAt,
		LastUsedAt:             cred.LastUsedAt,
		RecoveryCodesRemaining: count,
	}, nil
}

// --- helpers --------------------------------------------------------------

// validate runs the RFC 6238 verification against the secret. We
// delegate to pquerna/otp's totp.ValidateCustom so the algorithm,
// digits, and period parameters are explicit and consistent with
// what buildOtpauthURI encodes into the QR code.
func validate(code, secret string, now time.Time, skew uint) bool {
	ok, _ := totp.ValidateCustom(code, secret, now, totp.ValidateOpts{
		Period:    PeriodSeconds,
		Skew:      skew,
		Digits:    Digits,
		Algorithm: otp.AlgorithmSHA1,
	})
	return ok
}

// buildOtpauthURI assembles the otpauth:// URI the authenticator
// app expects. We build it by string concatenation rather than
// using url.URL because the RFC 4226 spec specifies a particular
// label-encoding convention ("issuer:account") that net/url's
// path-escaping would mangle into "issuer%3Aaccount".
func buildOtpauthURI(issuer, account, secret string) string {
	label := otpauthEscape(issuer + ":" + account)
	return fmt.Sprintf(
		"otpauth://totp/%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		label,
		secret,
		otpauthEscape(issuer),
	)
}

// otpauthEscape applies the minimal percent-encoding the otpauth
// URI spec requires: spaces -> %20, and a handful of reserved
// characters. We deliberately do NOT escape the ':' in the label
// — authenticator apps parse "issuer:account" by splitting on the
// first ':' character.
func otpauthEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ':
			b.WriteString("%20")
		case '?', '#', '&', '=', '/', '%':
			b.WriteString(fmt.Sprintf("%%%02X", r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderQRCodePNG renders the otpauth URI as a 256x256 PNG and
// returns it base64-encoded. The base64 prefix is suitable for
// embedding directly in a data: URL by the frontend without further
// transformation.
func renderQRCodePNG(key *otp.Key) (string, error) {
	img, err := key.Image(256, 256)
	if err != nil {
		return "", fmt.Errorf("qr image: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode png: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// generateRecoveryCodes returns (plaintext codes, bcrypt hashes).
// The plaintext slice is shown to the user EXACTLY ONCE and then
// discarded; the hashes are persisted via InsertRecoveryCodes.
func generateRecoveryCodes(n int) ([]string, []string, error) {
	plaintexts := make([]string, 0, n)
	hashes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		code, err := generateOneRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		hash, err := appcrypto.HashPassword(code)
		if err != nil {
			return nil, nil, fmt.Errorf("bcrypt hash recovery code: %w", err)
		}
		plaintexts = append(plaintexts, code)
		hashes = append(hashes, string(hash))
	}
	return plaintexts, hashes, nil
}

// generateOneRecoveryCode draws RecoveryCodeGroups * RecoveryCodeGroupSize
// characters from recoveryAlphabet (using crypto/rand to avoid
// modulo bias via rejection sampling) and joins them with dashes.
func generateOneRecoveryCode() (string, error) {
	total := RecoveryCodeGroups * RecoveryCodeGroupSize
	chars := make([]byte, total)
	alphabetLen := byte(len(recoveryAlphabet))

	// Rejection-sampled rand draws so the alphabet's 31 chars are
	// uniform — a naive [0,255] % 31 has a 1.4% bias toward the
	// first 24 characters. The cap of 256 / 31 * 31 = 248 means
	// any draw >= 248 is rejected.
	threshold := byte(256 / int(alphabetLen) * int(alphabetLen))
	pos := 0
	for pos < total {
		buf := make([]byte, total-pos)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("rand read: %w", err)
		}
		for _, b := range buf {
			if b >= threshold {
				continue
			}
			chars[pos] = recoveryAlphabet[b%alphabetLen]
			pos++
			if pos == total {
				break
			}
		}
	}

	var b strings.Builder
	b.Grow(total + RecoveryCodeGroups - 1)
	for i := 0; i < RecoveryCodeGroups; i++ {
		if i > 0 {
			b.WriteByte('-')
		}
		b.Write(chars[i*RecoveryCodeGroupSize : (i+1)*RecoveryCodeGroupSize])
	}
	return b.String(), nil
}

// normalizeRecoveryCode lowercases and strips whitespace/dashes so
// the user can paste "Xb 4q 9Z-Pm-Tk" and have it match
// "xb-4q-9z-pm-tk". The canonical form (lower, dash-separated) is
// what was bcrypt-hashed; we MUST re-derive it here so
// CompareHashAndPassword finds a match.
func normalizeRecoveryCode(input string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(input) {
		if r == ' ' || r == '-' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	flat := b.String()
	// Re-insert dashes every RecoveryCodeGroupSize chars to
	// produce the canonical form.
	if len(flat) != RecoveryCodeGroups*RecoveryCodeGroupSize {
		return flat
	}
	var out strings.Builder
	out.Grow(len(flat) + RecoveryCodeGroups - 1)
	for i := 0; i < RecoveryCodeGroups; i++ {
		if i > 0 {
			out.WriteByte('-')
		}
		out.WriteString(flat[i*RecoveryCodeGroupSize : (i+1)*RecoveryCodeGroupSize])
	}
	return out.String()
}

// isWellFormedRecoveryCode validates the canonical form before any
// DB / bcrypt work. Cheap rejection of obvious junk avoids spending
// CPU on bcrypt comparisons for inputs that can't possibly match.
// Timing is not a concern here: the input's length and dash layout
// are not secret, and the alphabet is public.
func isWellFormedRecoveryCode(s string) bool {
	want := RecoveryCodeGroups*RecoveryCodeGroupSize + (RecoveryCodeGroups - 1)
	if len(s) != want {
		return false
	}
	pos := 0
	for i := 0; i < RecoveryCodeGroups; i++ {
		if i > 0 {
			if s[pos] != '-' {
				return false
			}
			pos++
		}
		for j := 0; j < RecoveryCodeGroupSize; j++ {
			if !strings.ContainsRune(recoveryAlphabet, rune(s[pos])) {
				return false
			}
			pos++
		}
	}
	return true
}
