package crypto

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// memKeyStore is an in-memory SigningKeyStore for exercising the
// KeyManager without a live Postgres. It mirrors the platform-wide
// (workspace_id IS NULL) semantics of the real store: Rotate retires
// the active key and inserts the new one as active.
type memKeyStore struct {
	mu   sync.Mutex
	recs []SigningKeyRecord
}

func (m *memKeyStore) ListKeys(_ context.Context) ([]SigningKeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SigningKeyRecord, len(m.recs))
	copy(out, m.recs)
	return out, nil
}

func (m *memKeyStore) Rotate(_ context.Context, rec SigningKeyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for i := range m.recs {
		if m.recs[i].IsActive {
			m.recs[i].IsActive = false
			m.recs[i].RotatedAt = &now
		}
	}
	// newest-first ordering to match the Postgres store's ORDER BY.
	m.recs = append([]SigningKeyRecord{rec}, m.recs...)
	return nil
}

// testCodec returns an AES-GCM codec with a fixed 32-byte key so the
// private-key encryption path is genuinely exercised.
func testCodec(t *testing.T) *Codec {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	c, err := NewAESGCMCodec(key)
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	return c
}

func makeClaims(ttl time.Duration) jwt.Claims {
	now := time.Now().UTC()
	return &jwt.RegisteredClaims{
		Subject:   "user-123",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
}

func TestKeyManager_SignVerifyRoundTrip_ES256(t *testing.T) {
	ctx := context.Background()
	km, err := NewKeyManager(ctx, &memKeyStore{}, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	// No keys yet → HS256 fallback.
	if got := km.Algorithm(); got != AlgHS256 {
		t.Fatalf("algorithm before rotate = %q, want %q", got, AlgHS256)
	}
	if _, err := km.RotateKey(ctx); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got := km.Algorithm(); got != AlgES256 {
		t.Fatalf("algorithm after rotate = %q, want %q", got, AlgES256)
	}

	signed, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	var parsed jwt.RegisteredClaims
	tok, err := km.Parse(signed, &parsed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !tok.Valid {
		t.Fatal("token not valid")
	}
	if tok.Method.Alg() != "ES256" {
		t.Fatalf("alg = %q, want ES256", tok.Method.Alg())
	}
	if parsed.Subject != "user-123" {
		t.Fatalf("subject = %q, want user-123", parsed.Subject)
	}
	if _, ok := tok.Header["kid"]; !ok {
		t.Fatal("expected kid header on ES256 token")
	}
}

func TestKeyManager_RotationKeepsOldTokenValid(t *testing.T) {
	ctx := context.Background()
	km, err := NewKeyManager(ctx, &memKeyStore{}, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}

	first, err := km.RotateKey(ctx)
	if err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	oldToken, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign with first key: %v", err)
	}

	// Rotate to a brand-new key; the first key is retired but retained.
	second, err := km.RotateKey(ctx)
	if err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected a new key id after rotation")
	}

	// Token signed by the retired key still verifies.
	if _, err := km.Parse(oldToken, &jwt.RegisteredClaims{}); err != nil {
		t.Fatalf("old token failed to verify after rotation: %v", err)
	}

	// New tokens are signed by the new active key and verify too.
	newToken, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign with second key: %v", err)
	}
	newParsed, err := km.Parse(newToken, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("new token failed to verify: %v", err)
	}
	if newParsed.Header["kid"] != second.ID.String() {
		t.Fatalf("new token kid = %v, want %v", newParsed.Header["kid"], second.ID.String())
	}
}

func TestKeyManager_HS256Fallback(t *testing.T) {
	ctx := context.Background()
	// Nil store → no asymmetric keys → HS256-only.
	km, err := NewKeyManager(ctx, nil, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	if got := km.Algorithm(); got != AlgHS256 {
		t.Fatalf("algorithm = %q, want %q", got, AlgHS256)
	}

	signed, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tok, err := km.Parse(signed, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.Method.Alg() != "HS256" {
		t.Fatalf("alg = %q, want HS256", tok.Method.Alg())
	}

	// A token minted directly with the same HS256 secret (mimicking the
	// legacy middleware path) verifies through the KeyManager too.
	legacy := jwt.NewWithClaims(jwt.SigningMethodHS256, makeClaims(time.Hour))
	legacyStr, err := legacy.SignedString([]byte("hs-secret"))
	if err != nil {
		t.Fatalf("legacy sign: %v", err)
	}
	if _, err := km.Parse(legacyStr, &jwt.RegisteredClaims{}); err != nil {
		t.Fatalf("legacy HS256 token failed to verify: %v", err)
	}
}

func TestKeyManager_ForceHS256IgnoresActiveKey(t *testing.T) {
	ctx := context.Background()
	store := &memKeyStore{}
	// Pre-seed an active ES256 key, then construct a manager pinned to
	// HS256: it must still sign HS256 even though a key exists.
	seed, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("seed manager: %v", err)
	}
	if _, err := seed.RotateKey(ctx); err != nil {
		t.Fatalf("seed rotate: %v", err)
	}

	km, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgHS256)
	if err != nil {
		t.Fatalf("forced HS256 manager: %v", err)
	}
	if got := km.Algorithm(); got != AlgHS256 {
		t.Fatalf("algorithm = %q, want %q", got, AlgHS256)
	}
	signed, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tok, err := km.Parse(signed, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.Method.Alg() != "HS256" {
		t.Fatalf("alg = %q, want HS256", tok.Method.Alg())
	}
}

func TestKeyManager_ForceHS256StillVerifiesES256(t *testing.T) {
	ctx := context.Background()
	store := &memKeyStore{}
	// Mint an ES256 token via an auto manager (simulating tokens issued
	// while ES256 was active).
	auto, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("auto manager: %v", err)
	}
	if _, err := auto.RotateKey(ctx); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	es256Token, err := auto.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign es256: %v", err)
	}

	// Operator rolls signing back to HS256 over the same key table.
	// Verification must still accept the in-flight ES256 token so users
	// are not locked out until their tokens expire.
	km, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgHS256)
	if err != nil {
		t.Fatalf("forced HS256 manager: %v", err)
	}
	if got := km.Algorithm(); got != AlgHS256 {
		t.Fatalf("algorithm = %q, want %q", got, AlgHS256)
	}
	tok, err := km.Parse(es256Token, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("forced-HS256 manager failed to verify in-flight ES256 token: %v", err)
	}
	if tok.Method.Alg() != "ES256" {
		t.Fatalf("alg = %q, want ES256", tok.Method.Alg())
	}
}

func TestKeyManager_RotateWithoutStoreFails(t *testing.T) {
	ctx := context.Background()
	km, err := NewKeyManager(ctx, nil, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	if _, err := km.RotateKey(ctx); err == nil {
		t.Fatal("expected error rotating without a store")
	}
}

// guard against accidental UUID collisions in the fake store helper.
func TestMemKeyStore_RotatePreservesHistory(t *testing.T) {
	ctx := context.Background()
	store := &memKeyStore{}
	_ = store.Rotate(ctx, SigningKeyRecord{ID: uuid.New(), IsActive: true})
	_ = store.Rotate(ctx, SigningKeyRecord{ID: uuid.New(), IsActive: true})
	recs, _ := store.ListKeys(ctx)
	if len(recs) != 2 {
		t.Fatalf("len(recs) = %d, want 2", len(recs))
	}
	active := 0
	for _, r := range recs {
		if r.IsActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active keys = %d, want 1", active)
	}
}
