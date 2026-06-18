package crypto

import (
	"context"
	"encoding/base64"
	"sync"
	"sync/atomic"
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

func TestKeyManager_ForceES256ErrorsWithoutActiveKey(t *testing.T) {
	ctx := context.Background()
	// JWT_ALGORITHM=ES256 is a compliance requirement: with no active
	// asymmetric key yet, Sign must refuse rather than silently mint an
	// HS256 token, even though an HS256 secret is configured.
	km, err := NewKeyManager(ctx, &memKeyStore{}, testCodec(t), "hs-secret", AlgES256)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	if _, err := km.Sign(makeClaims(time.Hour)); err == nil {
		t.Fatal("expected Sign to error under AlgES256 with no active key, got nil")
	}

	// After a key is rotated in, ES256 signing succeeds.
	if _, err := km.RotateKey(ctx); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	signed, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign after rotate: %v", err)
	}
	tok, err := km.Parse(signed, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.Method.Alg() != "ES256" {
		t.Fatalf("alg = %q, want ES256", tok.Method.Alg())
	}
}

func TestKeyManager_HMACVerificationDisabledRejectsHS256(t *testing.T) {
	ctx := context.Background()
	// Production profile: signing is ES256-only (AlgES256) and HS256
	// verification is disabled. A token forged with a leaked
	// JWT_SECRET must NOT verify, while ES256 tokens still do.
	store := &memKeyStore{}
	km, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgES256, WithHMACVerificationDisabled())
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	if _, err := km.RotateKey(ctx); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// A token forged with the (leaked) HS256 secret must be rejected.
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, makeClaims(time.Hour))
	forgedStr, err := forged.SignedString([]byte("hs-secret"))
	if err != nil {
		t.Fatalf("forge HS256: %v", err)
	}
	if _, err := km.Parse(forgedStr, &jwt.RegisteredClaims{}); err == nil {
		t.Fatal("expected HS256 token to be rejected in asymmetric-only mode, got nil error")
	}

	// A legitimately ES256-signed token must still verify.
	signed, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tok, err := km.Parse(signed, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("parse ES256: %v", err)
	}
	if tok.Method.Alg() != "ES256" {
		t.Fatalf("alg = %q, want ES256", tok.Method.Alg())
	}
}

func TestKeyManager_AutoFallsBackToHS256WithoutActiveKey(t *testing.T) {
	ctx := context.Background()
	// AlgAuto (the default) must keep the backward-compatible HS256
	// fallback when no asymmetric key exists — the enforcement only
	// applies to AlgES256.
	km, err := NewKeyManager(ctx, &memKeyStore{}, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	signed, err := km.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("auto sign should fall back to HS256, got error: %v", err)
	}
	tok, err := km.Parse(signed, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.Method.Alg() != "HS256" {
		t.Fatalf("alg = %q, want HS256", tok.Method.Alg())
	}
}

func TestKeyManager_ReloadPropagatesRotationAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	store := &memKeyStore{}
	// Two managers over the same store model two replicas behind a load
	// balancer. replicaA serves the rotate; replicaB only learns the
	// new key after a reload (the cross-replica propagation path).
	replicaA, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("replicaA: %v", err)
	}
	replicaB, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("replicaB: %v", err)
	}

	rec, err := replicaA.RotateKey(ctx)
	if err != nil {
		t.Fatalf("rotate on A: %v", err)
	}
	tokenFromA, err := replicaA.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign on A: %v", err)
	}

	// Before B reloads, it has never seen the new kid → verification
	// fails (this is exactly the 401 this test pins).
	if _, err := replicaB.Parse(tokenFromA, &jwt.RegisteredClaims{}); err == nil {
		t.Fatal("expected replicaB to reject token from new key before reload")
	}

	// After B reloads (what RefreshLoop does on a timer), it accepts it.
	if err := replicaB.Reload(ctx); err != nil {
		t.Fatalf("reload on B: %v", err)
	}
	tok, err := replicaB.Parse(tokenFromA, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("replicaB still rejects token after reload: %v", err)
	}
	if tok.Header["kid"] != rec.ID.String() {
		t.Fatalf("kid = %v, want %v", tok.Header["kid"], rec.ID.String())
	}
}

func TestKeyManager_RefreshLoopPicksUpRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &memKeyStore{}
	replicaA, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("replicaA: %v", err)
	}
	replicaB, err := NewKeyManager(ctx, store, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("replicaB: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		replicaB.RefreshLoop(ctx, 5*time.Millisecond)
	}()

	if _, err := replicaA.RotateKey(ctx); err != nil {
		t.Fatalf("rotate on A: %v", err)
	}
	tokenFromA, err := replicaA.Sign(makeClaims(time.Hour))
	if err != nil {
		t.Fatalf("sign on A: %v", err)
	}

	// The background loop should converge B onto the new key set well
	// within this deadline without any manual reload.
	deadline := time.After(2 * time.Second)
	for {
		if _, err := replicaB.Parse(tokenFromA, &jwt.RegisteredClaims{}); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("RefreshLoop did not propagate rotation within deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Cancelling the context must terminate the loop (graceful shutdown).
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshLoop did not exit after context cancellation")
	}
}

func TestKeyManager_RefreshLoopDisabledReturnsImmediately(t *testing.T) {
	km, err := NewKeyManager(context.Background(), &memKeyStore{}, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A non-positive interval disables the loop: it must return at
		// once even though the context is never cancelled.
		km.RefreshLoop(context.Background(), 0)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RefreshLoop(interval=0) should return immediately")
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

// gatedStore wraps a SigningKeyStore and pauses exactly one ListKeys
// call (the first after Arm) inside the store read: it signals via
// reading once the snapshot has been taken, then blocks until release
// is closed. This lets a test pin one reload in mid-flight to observe
// the serialization invariant that reloadMu enforces.
type gatedStore struct {
	inner   SigningKeyStore
	armed   atomic.Bool
	reading chan struct{}
	release chan struct{}
}

func (g *gatedStore) ListKeys(ctx context.Context) ([]SigningKeyRecord, error) {
	if g.armed.CompareAndSwap(true, false) {
		recs, err := g.inner.ListKeys(ctx)
		close(g.reading)
		<-g.release
		return recs, err
	}
	return g.inner.ListKeys(ctx)
}

func (g *gatedStore) Rotate(ctx context.Context, rec SigningKeyRecord) error {
	return g.inner.Rotate(ctx, rec)
}

// TestKeyManager_ReloadIsSerialized is a regression test for the
// concurrent-reload race: reload reads the store and rebuilds its maps,
// and before the fix only the final assignment was guarded — so a slow
// reload holding a stale snapshot could swap in last and clobber a
// fresher concurrent reload. The fix serializes the entire reload under
// reloadMu. This test pins reload A inside its store read (holding
// reloadMu) and asserts reload B cannot complete until A releases —
// i.e. reloads are mutually exclusive across the whole read→swap. Without
// the fix, B sails through its (un-gated) read and completes immediately,
// failing the test.
func TestKeyManager_ReloadIsSerialized(t *testing.T) {
	ctx := context.Background()
	g := &gatedStore{
		inner:   &memKeyStore{},
		reading: make(chan struct{}),
		release: make(chan struct{}),
	}
	// Construction reload runs before arming, so it isn't gated.
	km, err := NewKeyManager(ctx, g, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}

	g.armed.Store(true)
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		_ = km.Reload(ctx)
	}()
	<-g.reading // A has read the store and is parked, holding reloadMu.

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_ = km.Reload(ctx)
	}()

	select {
	case <-bDone:
		t.Fatal("second reload completed while the first was mid-flight; reloads are not serialized (reloadMu missing)")
	case <-time.After(200 * time.Millisecond):
		// Expected: B is blocked on reloadMu held by A.
	}

	close(g.release) // Let A finish and drop reloadMu.
	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second reload did not complete after the first released reloadMu")
	}
	<-aDone
}

// TestKeyManager_ConcurrentReloadAndRotate exercises RotateKey racing
// many Reload calls under -race, and asserts the manager converges on a
// loaded ES256 signing key with a kid present in the verify map (i.e. no
// reload left a torn or stale set installed). Pair with `go test -race`.
func TestKeyManager_ConcurrentReloadAndRotate(t *testing.T) {
	ctx := context.Background()
	km, err := NewKeyManager(ctx, &memKeyStore{}, testCodec(t), "hs-secret", AlgAuto)
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = km.Reload(ctx)
			}
		}()
	}
	for i := 0; i < 5; i++ {
		if _, err := km.RotateKey(ctx); err != nil {
			t.Errorf("rotate: %v", err)
		}
	}
	wg.Wait()

	// One last reload to settle on the final persisted state, then verify
	// the active signing key is consistent with the verify map.
	if err := km.Reload(ctx); err != nil {
		t.Fatalf("final reload: %v", err)
	}
	if got := km.Algorithm(); got != AlgES256 {
		t.Fatalf("algorithm = %q, want ES256 after rotations", got)
	}
	km.mu.RLock()
	_, ok := km.verifyKeys[km.signingKID]
	km.mu.RUnlock()
	if !ok {
		t.Fatal("active signing kid not present in verify map after concurrent reload/rotate")
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
