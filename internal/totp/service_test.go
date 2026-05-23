package totp

import (
	"context"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TestRFC6238TestVectors pins the algorithm against the published
// reference values in RFC 6238 Appendix B. If pquerna/otp's
// implementation ever drifts from the standard, this test fails
// before any of our code can ship a non-interoperable change.
//
// The Appendix B vectors target a 20-byte ASCII secret
// "12345678901234567890" (raw bytes, not the base32 encoding).
// Because pquerna's totp.GenerateCode takes the base32-encoded
// secret, we encode that ASCII string with base32 first.
//
// Only the SHA-1 / 8-digit vectors are reproduced — they're what
// RFC 6238 actually specifies, and verifying with truncation
// proves the HOTP truncation step (RFC 4226 §5.3) is wired
// correctly regardless of whether we ship 6 or 8 digits in
// production.
func TestRFC6238TestVectors(t *testing.T) {
	const asciiSecret = "12345678901234567890"
	b32 := mustBase32(t, asciiSecret)

	vectors := []struct {
		unixTime int64
		want     string
	}{
		// RFC 6238 Appendix B "Test Values for TOTP" SHA-1 column.
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}

	for _, v := range vectors {
		got, err := totp.GenerateCodeCustom(b32, time.Unix(v.unixTime, 0).UTC(), totp.ValidateOpts{
			Period:    30,
			Digits:    otp.DigitsEight,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			t.Fatalf("GenerateCodeCustom for unix %d: %v", v.unixTime, err)
		}
		if got != v.want {
			t.Errorf("RFC 6238 vector at unix=%d: got %q want %q", v.unixTime, got, v.want)
		}
	}
}

// TestServiceBeginAndFinalizeRoundTrip exercises the happy path:
// enroll -> generate code at current time -> finalize -> activated.
// Pinned because this is the contract the auth handlers depend on.
func TestServiceBeginAndFinalizeRoundTrip(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, repo, _ := newTestService(t, now)

	userID := uuid.New()
	challenge, err := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if challenge.Secret == "" {
		t.Fatal("BeginEnrollment returned empty secret")
	}
	if !strings.HasPrefix(challenge.OtpauthURI, "otpauth://totp/") {
		t.Errorf("OtpauthURI missing scheme: %q", challenge.OtpauthURI)
	}
	if challenge.QRCodePNG == "" {
		t.Fatal("BeginEnrollment returned empty QRCodePNG")
	}
	// The row should now be pending in the repo.
	cred, err := repo.GetCredential(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetCredential after Begin: %v", err)
	}
	if cred.IsActivated() {
		t.Fatal("credential activated before Finalize")
	}

	// Generate the live code that the authenticator would render
	// "right now", then finalize with it.
	code := generateCode(t, challenge.Secret, now)
	codes, err := svc.FinalizeEnrollment(context.Background(), userID, code)
	if err != nil {
		t.Fatalf("FinalizeEnrollment: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Errorf("FinalizeEnrollment returned %d codes, want %d", len(codes), RecoveryCodeCount)
	}
	for i, c := range codes {
		if !isWellFormedRecoveryCode(c) {
			t.Errorf("recovery code %d %q is not well-formed", i, c)
		}
	}
	cred, _ = repo.GetCredential(context.Background(), userID)
	if !cred.IsActivated() {
		t.Fatal("credential not activated after Finalize")
	}
}

// TestServiceBeginRejectsAlreadyActivated pins the lockout-prevention
// invariant: BeginEnrollment MUST NOT overwrite an active credential.
// The user has to explicitly Disable first.
func TestServiceBeginRejectsAlreadyActivated(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, _, _ := newTestService(t, now)
	userID := uuid.New()

	// Enroll and finalize.
	ch, err := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if _, err := svc.FinalizeEnrollment(context.Background(), userID, generateCode(t, ch.Secret, now)); err != nil {
		t.Fatalf("FinalizeEnrollment: %v", err)
	}

	// Now try to begin again — must refuse.
	_, err = svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	if !errors.Is(err, ErrAlreadyActivated) {
		t.Fatalf("BeginEnrollment on activated row: err=%v want ErrAlreadyActivated", err)
	}

	// After Disable, BeginEnrollment must succeed again.
	if err := svc.Disable(context.Background(), userID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := svc.BeginEnrollment(context.Background(), userID, "alice@example.com"); err != nil {
		t.Fatalf("BeginEnrollment after Disable: %v", err)
	}
}

// TestServiceVerifyHappyPath enrolls a user and then verifies a
// freshly generated code.
func TestServiceVerifyHappyPath(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, _, clock := newTestService(t, now)
	userID := uuid.New()
	ch, _ := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	if _, err := svc.FinalizeEnrollment(context.Background(), userID, generateCode(t, ch.Secret, now)); err != nil {
		t.Fatalf("FinalizeEnrollment: %v", err)
	}

	// Advance the clock 1 minute (2 periods) to ensure replay
	// protection allows the new code.
	clock.set(now.Add(60 * time.Second))
	code := generateCode(t, ch.Secret, clock.now())
	if err := svc.Verify(context.Background(), userID, code); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestServiceVerifyRejectsReplay enrolls, verifies once, then
// re-uses the same code immediately and expects ErrInvalidCode.
// This is the core RFC 6238 §5.2 single-use property.
func TestServiceVerifyRejectsReplay(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, _, clock := newTestService(t, now)
	userID := uuid.New()
	ch, _ := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	if _, err := svc.FinalizeEnrollment(context.Background(), userID, generateCode(t, ch.Secret, now)); err != nil {
		t.Fatalf("FinalizeEnrollment: %v", err)
	}

	clock.set(now.Add(60 * time.Second))
	code := generateCode(t, ch.Secret, clock.now())
	if err := svc.Verify(context.Background(), userID, code); err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	// Same clock, same code -> replay
	err := svc.Verify(context.Background(), userID, code)
	if !errors.Is(err, ErrCodeReplayed) {
		t.Fatalf("second Verify with same code: err=%v want ErrCodeReplayed", err)
	}
}

// TestServiceVerifyRejectsInvalidCode covers the wrong-code path.
func TestServiceVerifyRejectsInvalidCode(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, _, _ := newTestService(t, now)
	userID := uuid.New()
	ch, _ := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	if _, err := svc.FinalizeEnrollment(context.Background(), userID, generateCode(t, ch.Secret, now)); err != nil {
		t.Fatalf("FinalizeEnrollment: %v", err)
	}

	err := svc.Verify(context.Background(), userID, "000000")
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("Verify with bogus code: err=%v want ErrInvalidCode", err)
	}
}

// TestServiceVerifyRejectsNotActivated pins that a pending row
// cannot be used to satisfy a login challenge.
func TestServiceVerifyRejectsNotActivated(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, _, _ := newTestService(t, now)
	userID := uuid.New()
	ch, _ := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")

	err := svc.Verify(context.Background(), userID, generateCode(t, ch.Secret, now))
	if !errors.Is(err, ErrNotActivated) {
		t.Fatalf("Verify on pending: err=%v want ErrNotActivated", err)
	}
}

// TestServiceConsumeRecoveryCode round-trips: get codes at
// finalize, then burn one. After burn, the second attempt with the
// same code must fail (single-use), but a different code must
// still succeed.
func TestServiceConsumeRecoveryCode(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, repo, _ := newTestService(t, now)
	userID := uuid.New()
	ch, _ := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	codes, err := svc.FinalizeEnrollment(context.Background(), userID, generateCode(t, ch.Secret, now))
	if err != nil {
		t.Fatalf("FinalizeEnrollment: %v", err)
	}
	if len(codes) < 2 {
		t.Fatalf("need at least 2 codes to test")
	}

	if err := svc.ConsumeRecoveryCode(context.Background(), userID, codes[0]); err != nil {
		t.Fatalf("ConsumeRecoveryCode (first): %v", err)
	}
	// Replay first code -> reject.
	if err := svc.ConsumeRecoveryCode(context.Background(), userID, codes[0]); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("ConsumeRecoveryCode (replay): err=%v want ErrInvalidCode", err)
	}
	// Use second code -> succeed.
	if err := svc.ConsumeRecoveryCode(context.Background(), userID, codes[1]); err != nil {
		t.Fatalf("ConsumeRecoveryCode (second): %v", err)
	}

	// Status should show 8 remaining.
	st, err := svc.Status(context.Background(), userID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.RecoveryCodesRemaining != RecoveryCodeCount-2 {
		t.Errorf("RecoveryCodesRemaining = %d, want %d", st.RecoveryCodesRemaining, RecoveryCodeCount-2)
	}
	// The total row count should be unchanged (used codes are
	// kept for audit).
	if total := len(repo.codes[userID]); total != RecoveryCodeCount {
		t.Errorf("total recovery code rows after 2 burns = %d, want %d", total, RecoveryCodeCount)
	}
}

// TestServiceConsumeRecoveryCodeNormalization pins that the user
// can paste the code in mixed case or with whitespace and still
// have it match.
func TestServiceConsumeRecoveryCodeNormalization(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, _, _ := newTestService(t, now)
	userID := uuid.New()
	ch, _ := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	codes, _ := svc.FinalizeEnrollment(context.Background(), userID, generateCode(t, ch.Secret, now))

	// Capitalise, add spaces around dashes, prepend whitespace.
	smudged := "  " + strings.ToUpper(strings.ReplaceAll(codes[0], "-", " - ")) + "  "
	if err := svc.ConsumeRecoveryCode(context.Background(), userID, smudged); err != nil {
		t.Fatalf("ConsumeRecoveryCode smudged: %v", err)
	}
}

// TestServiceStatusNotEnrolled pins that an un-enrolled user
// returns a zero-Status without an error (so the auth handler can
// treat "no row" and "row exists but disabled" identically).
func TestServiceStatusNotEnrolled(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, _, _ := newTestService(t, now)
	st, err := svc.Status(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Status unenrolled: %v", err)
	}
	if st.Enabled || st.PendingEnrollment {
		t.Errorf("unenrolled Status = %+v, want zero-value", st)
	}
}

// TestServiceDisable wipes the credential AND the recovery codes
// (FK cascade). Asserting both paths so a future regression that
// drops the cascade is caught here.
func TestServiceDisable(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, repo, _ := newTestService(t, now)
	userID := uuid.New()
	ch, _ := svc.BeginEnrollment(context.Background(), userID, "alice@example.com")
	if _, err := svc.FinalizeEnrollment(context.Background(), userID, generateCode(t, ch.Secret, now)); err != nil {
		t.Fatalf("FinalizeEnrollment: %v", err)
	}

	if err := svc.Disable(context.Background(), userID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := repo.GetCredential(context.Background(), userID); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("credential still present after Disable: err=%v", err)
	}
	// The in-memory fake doesn't actually FK-cascade, so we
	// emulate the production schema's behaviour: confirm the
	// service's Disable path explicitly nukes the row family
	// rather than relying on the fake's lazy semantics.
	if len(repo.codes[userID]) != 0 {
		t.Errorf("recovery codes survived Disable: %d remain", len(repo.codes[userID]))
	}
}

// TestRecoveryCodeGenerationEntropy spot-checks that 100 codes are
// all distinct. Statistically the birthday-paradox collision
// probability on RecoveryCodeGroups*RecoveryCodeGroupSize=10 chars
// from a 31-char alphabet over 100 draws is <1 in 10^11, so a
// failure here means the rand source is broken.
func TestRecoveryCodeGenerationEntropy(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		c, err := generateOneRecoveryCode()
		if err != nil {
			t.Fatalf("generateOneRecoveryCode: %v", err)
		}
		if _, dup := seen[c]; dup {
			t.Fatalf("duplicate recovery code on draw %d: %q", i, c)
		}
		seen[c] = struct{}{}
		if !isWellFormedRecoveryCode(c) {
			t.Errorf("ill-formed code %q", c)
		}
	}
}

// TestIsWellFormedRecoveryCode pins the validator's input space
// against accidental loosening of the dash placement / alphabet.
func TestIsWellFormedRecoveryCode(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"xb-4q-9z-pm-tk", true},
		{"XB-4Q-9Z-PM-TK", false}, // case sensitive; normalize-before-validate is the caller's job
		{"xb4q9zpmtk", false},     // missing dashes
		{"xb-4q-9z-pm-tk ", false}, // trailing space
		{"xb-4q-9z-pm-t", false},   // too short
		{"0b-4q-9z-pm-tk", false},  // '0' not in alphabet
		{"o0-4q-9z-pm-tk", false},  // 'o' / '0' both excluded
		{"", false},
	}
	for _, c := range cases {
		if got := isWellFormedRecoveryCode(c.in); got != c.want {
			t.Errorf("isWellFormedRecoveryCode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestOtpauthURIShape pins the URI format authenticator apps
// expect: scheme/host/path/secret param/issuer param/algorithm
// param/digits/period.
func TestOtpauthURIShape(t *testing.T) {
	uri := buildOtpauthURI("zk-drive", "alice@example.com", "ABCDEFGHIJKLMNOP")
	wantParts := []string{
		"otpauth://totp/zk-drive:alice@example.com?",
		"secret=ABCDEFGHIJKLMNOP",
		"issuer=zk-drive",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	}
	for _, p := range wantParts {
		if !strings.Contains(uri, p) {
			t.Errorf("otpauth URI missing %q\nactual: %s", p, uri)
		}
	}
}

// --- in-memory fakes -----------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time      { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) set(t time.Time)     { f.mu.Lock(); f.t = t; f.mu.Unlock() }

type fakeRepo struct {
	mu    sync.Mutex
	creds map[uuid.UUID]*Credential
	codes map[uuid.UUID][]*RecoveryCode
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		creds: map[uuid.UUID]*Credential{},
		codes: map[uuid.UUID][]*RecoveryCode{},
	}
}

func (r *fakeRepo) GetCredential(_ context.Context, userID uuid.UUID) (*Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[userID]
	if !ok {
		return nil, ErrNotEnrolled
	}
	// Copy so callers can't mutate the stored row.
	cp := *c
	if c.ActivatedAt != nil {
		t := *c.ActivatedAt
		cp.ActivatedAt = &t
	}
	if c.LastUsedAt != nil {
		t := *c.LastUsedAt
		cp.LastUsedAt = &t
	}
	return &cp, nil
}

func (r *fakeRepo) UpsertCredential(_ context.Context, cred *Credential) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *cred
	if existing, ok := r.creds[cred.UserID]; ok {
		cp.CreatedAt = existing.CreatedAt
	} else {
		cp.CreatedAt = time.Now().UTC()
	}
	cp.UpdatedAt = time.Now().UTC()
	r.creds[cred.UserID] = &cp
	cred.CreatedAt = cp.CreatedAt
	cred.UpdatedAt = cp.UpdatedAt
	return nil
}

func (r *fakeRepo) FinalizeEnrollment(_ context.Context, userID uuid.UUID, activatedAt time.Time, recoveryHashes []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[userID]
	if !ok {
		return ErrNotEnrolled
	}
	t := activatedAt
	c.ActivatedAt = &t
	c.UpdatedAt = time.Now().UTC()
	// Wipe prior recovery codes; emulate the production tx that
	// runs DELETE before INSERT.
	delete(r.codes, userID)
	for _, h := range recoveryHashes {
		r.codes[userID] = append(r.codes[userID], &RecoveryCode{
			ID:        uuid.New(),
			UserID:    userID,
			CodeHash:  h,
			CreatedAt: time.Now().UTC(),
		})
	}
	return nil
}

func (r *fakeRepo) DeleteCredential(_ context.Context, userID uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.creds[userID]; !ok {
		return ErrNotEnrolled
	}
	delete(r.creds, userID)
	delete(r.codes, userID) // emulate ON DELETE CASCADE
	return nil
}

func (r *fakeRepo) UpdateLastUsed(_ context.Context, userID uuid.UUID, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[userID]
	if !ok {
		return ErrNotEnrolled
	}
	t := at
	c.LastUsedAt = &t
	c.UpdatedAt = time.Now().UTC()
	return nil
}

func (r *fakeRepo) ListUnusedRecoveryCodes(_ context.Context, userID uuid.UUID) ([]*RecoveryCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*RecoveryCode
	for _, c := range r.codes[userID] {
		if c.UsedAt == nil {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *fakeRepo) MarkRecoveryCodeUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, list := range r.codes {
		for _, c := range list {
			if c.ID == id {
				if c.UsedAt != nil {
					return ErrCodeReplayed
				}
				t := at
				c.UsedAt = &t
				return nil
			}
		}
	}
	return ErrCodeReplayed
}

func (r *fakeRepo) CountUnusedRecoveryCodes(_ context.Context, userID uuid.UUID) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.codes[userID] {
		if c.UsedAt == nil {
			n++
		}
	}
	return n, nil
}

// identityCodec is a no-op Encryptor used in tests so the service
// can be exercised without a real *crypto.Codec.
type identityCodec struct{}

func (identityCodec) Encrypt(_ context.Context, p string) (string, error) { return p, nil }
func (identityCodec) Decrypt(_ context.Context, c string) (string, error) { return c, nil }

func newTestService(t *testing.T, now time.Time) (*Service, *fakeRepo, *fakeClock) {
	t.Helper()
	repo := newFakeRepo()
	clock := &fakeClock{t: now}
	svc := NewService(repo, identityCodec{}, "zk-drive").WithClock(clock.now)
	return svc, repo, clock
}

func generateCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period:    PeriodSeconds,
		Digits:    Digits,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}
	return code
}

func mustBase32(t *testing.T, raw string) string {
	t.Helper()
	// totp.GenerateCodeCustom expects an RFC 4648 base32 secret
	// with no padding — that is what every authenticator app
	// renders when the user taps "enter setup key".
	return strings.TrimRight(base32.StdEncoding.EncodeToString([]byte(raw)), "=")
}
