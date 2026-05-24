package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	appcrypto "github.com/kennguy3n/zk-drive/internal/crypto"
)

// fakeRepo is a minimal in-memory Repository implementing only the
// methods MaybeRehashPassword exercises. Other methods panic so a
// future test that accidentally hits them is loud rather than
// silently passing.
type fakeRepo struct {
	updateCalls   int
	updateHashes  map[uuid.UUID]string
	updateErr     error
	updateUserIDs []uuid.UUID
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{updateHashes: make(map[uuid.UUID]string)}
}

func (f *fakeRepo) UpdatePasswordHash(_ context.Context, userID uuid.UUID, hash string) error {
	f.updateCalls++
	f.updateUserIDs = append(f.updateUserIDs, userID)
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updateHashes[userID] = hash
	return nil
}

// The rest of the Repository interface is implemented as panicking
// stubs so a stray call would crash the test loudly. We only need
// UpdatePasswordHash for MaybeRehashPassword coverage.

func (f *fakeRepo) Create(context.Context, *User) error             { panic("unused") }
func (f *fakeRepo) CreateTx(context.Context, pgx.Tx, *User) error   { panic("unused") }
func (f *fakeRepo) GetByID(context.Context, uuid.UUID, uuid.UUID) (*User, error) {
	panic("unused")
}
func (f *fakeRepo) GetByEmail(context.Context, uuid.UUID, string) (*User, error) {
	panic("unused")
}
func (f *fakeRepo) GetByEmailAnyWorkspace(context.Context, string) (*User, error) {
	panic("unused")
}
func (f *fakeRepo) GetByAuthProvider(context.Context, string, string) (*User, error) {
	panic("unused")
}
func (f *fakeRepo) List(context.Context, uuid.UUID) ([]*User, error) { panic("unused") }
func (f *fakeRepo) UpdateLastLogin(context.Context, uuid.UUID, time.Time) error {
	panic("unused")
}
func (f *fakeRepo) Deactivate(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	panic("unused")
}
func (f *fakeRepo) UpdateRole(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("unused")
}
func (f *fakeRepo) LinkAuthProvider(context.Context, uuid.UUID, string, string) error {
	panic("unused")
}

func TestMaybeRehashPasswordUpgradesLowerCostHash(t *testing.T) {
	password := "correct horse battery staple"
	// Legacy hash at cost 10 (the bcrypt default before we bumped to 12).
	legacyHash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		t.Fatalf("setup: hash with cost 10: %v", err)
	}
	if appcrypto.PasswordHashCost <= 10 {
		t.Fatalf("PasswordHashCost (%d) must be > 10 for this test to be meaningful", appcrypto.PasswordHashCost)
	}

	repo := newFakeRepo()
	svc := NewService(repo)
	u := &User{ID: uuid.New(), PasswordHash: string(legacyHash)}

	if err := svc.MaybeRehashPassword(context.Background(), u, password); err != nil {
		t.Fatalf("MaybeRehashPassword: %v", err)
	}

	if repo.updateCalls != 1 {
		t.Fatalf("want 1 UpdatePasswordHash call, got %d", repo.updateCalls)
	}
	newHashStr, ok := repo.updateHashes[u.ID]
	if !ok {
		t.Fatalf("repo did not receive a new hash for user %s", u.ID)
	}
	newCost, err := bcrypt.Cost([]byte(newHashStr))
	if err != nil {
		t.Fatalf("inspect new hash cost: %v", err)
	}
	if newCost != appcrypto.PasswordHashCost {
		t.Fatalf("rehashed cost: want %d, got %d", appcrypto.PasswordHashCost, newCost)
	}
	// The new hash must still verify against the same plaintext.
	if err := bcrypt.CompareHashAndPassword([]byte(newHashStr), []byte(password)); err != nil {
		t.Fatalf("rehashed password no longer verifies: %v", err)
	}
	// In-memory user struct must be updated so subsequent calls in
	// the same request use the new hash.
	if u.PasswordHash != newHashStr {
		t.Fatalf("u.PasswordHash not updated in-memory")
	}
}

func TestMaybeRehashPasswordNoOpWhenCostMatchesCurrent(t *testing.T) {
	password := "secret"
	// Already at current cost — no rehash should occur.
	currentHash, err := appcrypto.HashPassword(password)
	if err != nil {
		t.Fatalf("setup: HashPassword: %v", err)
	}
	repo := newFakeRepo()
	svc := NewService(repo)
	u := &User{ID: uuid.New(), PasswordHash: string(currentHash)}
	originalHash := u.PasswordHash

	if err := svc.MaybeRehashPassword(context.Background(), u, password); err != nil {
		t.Fatalf("MaybeRehashPassword: %v", err)
	}
	if repo.updateCalls != 0 {
		t.Fatalf("want 0 UpdatePasswordHash calls (no-op), got %d", repo.updateCalls)
	}
	if u.PasswordHash != originalHash {
		t.Fatalf("u.PasswordHash unexpectedly mutated by no-op path")
	}
}

func TestMaybeRehashPasswordNoOpWhenCostExceedsCurrent(t *testing.T) {
	// Hashes created at a higher cost than the current constant
	// (e.g. by a future contributor experimenting locally with
	// cost 14 then rolling back to 12) must NOT be downgraded.
	password := "secret"
	highCostHash, err := bcrypt.GenerateFromPassword([]byte(password), appcrypto.PasswordHashCost+2)
	if err != nil {
		t.Fatalf("setup: hash with high cost: %v", err)
	}
	repo := newFakeRepo()
	svc := NewService(repo)
	u := &User{ID: uuid.New(), PasswordHash: string(highCostHash)}

	if err := svc.MaybeRehashPassword(context.Background(), u, password); err != nil {
		t.Fatalf("MaybeRehashPassword: %v", err)
	}
	if repo.updateCalls != 0 {
		t.Fatalf("rehash path must not downgrade a higher-cost hash, got %d update calls", repo.updateCalls)
	}
}

func TestMaybeRehashPasswordReturnsErrOnRepoFailure(t *testing.T) {
	password := "pw"
	legacyHash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	repo := newFakeRepo()
	sentinel := errors.New("db down")
	repo.updateErr = sentinel
	svc := NewService(repo)
	u := &User{ID: uuid.New(), PasswordHash: string(legacyHash)}
	originalHash := u.PasswordHash

	err = svc.MaybeRehashPassword(context.Background(), u, password)
	if err == nil {
		t.Fatalf("want non-nil error when repo.UpdatePasswordHash fails")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel error, got %v", err)
	}
	// On failure the in-memory hash must NOT be overwritten — a
	// caller that retries the rehash later still sees the original
	// hash to compute the cost diff against.
	if u.PasswordHash != originalHash {
		t.Fatalf("u.PasswordHash must remain untouched on repo failure")
	}
}

func TestMaybeRehashPasswordReturnsErrOnGarbageHash(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)
	u := &User{ID: uuid.New(), PasswordHash: "not-a-bcrypt-hash"}

	err := svc.MaybeRehashPassword(context.Background(), u, "anything")
	if err == nil {
		t.Fatalf("want non-nil error when stored hash is unparseable")
	}
	if repo.updateCalls != 0 {
		t.Fatalf("must not write a new hash when the existing one is unparseable, got %d update calls", repo.updateCalls)
	}
}
