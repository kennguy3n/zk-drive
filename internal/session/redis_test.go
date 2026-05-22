package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newTestStore spins up an in-process miniredis instance and returns
// a RedisSessionStore wired to it. Using miniredis keeps the test
// hermetic — CI doesn't need a real Redis.
func newTestStore(t *testing.T) (*RedisSessionStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisSessionStore(client), mr
}

func TestSessionRevocation(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	sessionID := uuid.NewString()

	if err := store.Set(ctx, sessionID, userID, wsID, time.Hour); err != nil {
		t.Fatalf("set: %v", err)
	}
	gotUser, gotWS, err := store.Get(ctx, wsID, sessionID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotUser != userID || gotWS != wsID {
		t.Fatalf("unexpected ids: user=%s ws=%s", gotUser, gotWS)
	}

	if err := store.Revoke(ctx, wsID, sessionID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := store.Get(ctx, wsID, sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound after revoke, got %v", err)
	}
}

func TestRevokeAllForUser(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	sessions := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, sid := range sessions {
		if err := store.Set(ctx, sid, userID, wsID, time.Hour); err != nil {
			t.Fatalf("set %s: %v", sid, err)
		}
	}

	otherUser := uuid.New()
	survivor := uuid.NewString()
	if err := store.Set(ctx, survivor, otherUser, wsID, time.Hour); err != nil {
		t.Fatalf("set survivor: %v", err)
	}

	if err := store.RevokeAllForUser(ctx, wsID, userID); err != nil {
		t.Fatalf("revoke-all: %v", err)
	}
	for _, sid := range sessions {
		if _, _, err := store.Get(ctx, wsID, sid); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("session %s should be revoked: %v", sid, err)
		}
	}
	if _, _, err := store.Get(ctx, wsID, survivor); err != nil {
		t.Fatalf("survivor session should still exist: %v", err)
	}
}

// TestUserSessionsIndexTTLNeverShrinks regression-tests the bug
// flagged in Devin Review #3150549347: a short-lived session must
// not shrink the user_sessions SET TTL, otherwise the index can
// expire before older long-lived sessions and RevokeAllForUser will
// silently miss them.
func TestUserSessionsIndexTTLNeverShrinks(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	longLived := uuid.NewString()
	shortLived := uuid.NewString()

	// Long-lived session first: SET TTL = 1 hour.
	if err := store.Set(ctx, longLived, userID, wsID, time.Hour); err != nil {
		t.Fatalf("set long-lived: %v", err)
	}
	// Short-lived session second: SET TTL must stay at ~1 hour, not
	// shrink to 1 minute.
	if err := store.Set(ctx, shortLived, userID, wsID, time.Minute); err != nil {
		t.Fatalf("set short-lived: %v", err)
	}

	// Advance 5 minutes — short-lived hash is gone, long-lived
	// hash and the SET must both still be alive so a subsequent
	// RevokeAllForUser can find the long-lived session.
	mr.FastForward(5 * time.Minute)

	if err := store.RevokeAllForUser(ctx, wsID, userID); err != nil {
		t.Fatalf("revoke-all: %v", err)
	}
	if _, _, err := store.Get(ctx, wsID, longLived); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("long-lived session should be revoked, got err=%v", err)
	}
}

// TestRevokeUserAndIsRevoked exercises the per-user cutoff path that
// powers WS-1's JWT revocation. The IsRevoked decision boundary is
// inclusive ("iat <= cutoff") so a token issued in the same second
// as the revocation is treated as revoked — the conservative choice
// for the race between login completion and logout call.
func TestRevokeUserAndIsRevoked(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	otherUser := uuid.New()
	otherWs := uuid.New()

	// Baseline: no cutoff key yet, IsRevoked must return false
	// without error so first-time auth is not silently 401.
	revoked, err := store.IsRevoked(ctx, wsID, userID, time.Now())
	if err != nil {
		t.Fatalf("is-revoked (no key): %v", err)
	}
	if revoked {
		t.Fatal("IsRevoked must be false when no cutoff exists")
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := store.RevokeUser(ctx, wsID, userID, now, time.Hour); err != nil {
		t.Fatalf("revoke-user: %v", err)
	}

	cases := []struct {
		name        string
		issuedAt    time.Time
		wantRevoked bool
	}{
		{"iat before cutoff", now.Add(-time.Minute), true},
		{"iat exactly at cutoff", now, true},
		{"iat one second after cutoff", now.Add(time.Second), false},
		{"iat well after cutoff", now.Add(time.Hour), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			revoked, err := store.IsRevoked(ctx, wsID, userID, tc.issuedAt)
			if err != nil {
				t.Fatalf("is-revoked: %v", err)
			}
			if revoked != tc.wantRevoked {
				t.Errorf("revoked: got %v, want %v", revoked, tc.wantRevoked)
			}
		})
	}

	// Cross-tenant isolation: revoking userID in wsID must not
	// affect (otherUser, otherWs) nor (userID, otherWs).
	if r, err := store.IsRevoked(ctx, otherWs, userID, now.Add(-time.Hour)); err != nil || r {
		t.Errorf("cross-workspace leak: got revoked=%v err=%v, want revoked=false", r, err)
	}
	if r, err := store.IsRevoked(ctx, wsID, otherUser, now.Add(-time.Hour)); err != nil || r {
		t.Errorf("cross-user leak: got revoked=%v err=%v, want revoked=false", r, err)
	}
}

// TestRevokeUserTTL pins that the cutoff key self-cleans after the
// TTL elapses, so we don't accumulate per-user state forever. Without
// this, every logout in the system would permanently leak a Redis key.
func TestRevokeUserTTL(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	now := time.Now().UTC()

	if err := store.RevokeUser(ctx, wsID, userID, now, 30*time.Second); err != nil {
		t.Fatalf("revoke-user: %v", err)
	}
	revoked, err := store.IsRevoked(ctx, wsID, userID, now.Add(-time.Minute))
	if err != nil || !revoked {
		t.Fatalf("immediately after revoke: revoked=%v err=%v", revoked, err)
	}

	mr.FastForward(time.Minute)

	revoked, err = store.IsRevoked(ctx, wsID, userID, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("post-ttl is-revoked: %v", err)
	}
	if revoked {
		t.Fatal("cutoff key should self-clean after TTL; got revoked=true")
	}
}

// TestRevokeUserAtomicMaxUpdate is the regression test for the
// last-writer-wins race flagged in Devin Review on PR #48: two
// concurrent revocations could (in principle) land out of order
// such that an older timestamp overwrites a newer one, moving the
// cutoff backwards and re-validating tokens an earlier revocation
// intended to reject.
//
// The Lua-script max-update path on RevokeUser eliminates the race:
// any RevokeUser call with `at` strictly less than the current
// cutoff is a no-op. We pin that here by writing newer-then-older
// and asserting the cutoff stays at the newer value.
func TestRevokeUserAtomicMaxUpdate(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	base := time.Now().UTC().Truncate(time.Second)

	if err := store.RevokeUser(ctx, wsID, userID, base, time.Hour); err != nil {
		t.Fatalf("revoke at base: %v", err)
	}
	// Older timestamp lands after — must NOT move the cutoff
	// backwards. Without the Lua guard, this would set the cutoff
	// to base-1m and tokens issued in [base-1m, base] would no
	// longer be considered revoked.
	older := base.Add(-time.Minute)
	if err := store.RevokeUser(ctx, wsID, userID, older, time.Hour); err != nil {
		t.Fatalf("revoke at older: %v", err)
	}
	// A token with iat = base - 30s was clearly issued before the
	// original revocation cutoff (base) and must therefore remain
	// revoked even though we attempted to write `older`.
	revoked, err := store.IsRevoked(ctx, wsID, userID, base.Add(-30*time.Second))
	if err != nil {
		t.Fatalf("is-revoked: %v", err)
	}
	if !revoked {
		t.Fatal("max-update violated: older RevokeUser moved cutoff backwards")
	}

	// Newer timestamp lands after — must move the cutoff forward.
	newer := base.Add(time.Minute)
	if err := store.RevokeUser(ctx, wsID, userID, newer, time.Hour); err != nil {
		t.Fatalf("revoke at newer: %v", err)
	}
	// A token with iat = newer - 30s is now within the revoked
	// window, while iat = newer + 1s sits beyond the cutoff and
	// must be considered valid.
	if r, _ := store.IsRevoked(ctx, wsID, userID, newer.Add(-30*time.Second)); !r {
		t.Error("post-newer: token before cutoff should be revoked")
	}
	if r, _ := store.IsRevoked(ctx, wsID, userID, newer.Add(time.Second)); r {
		t.Error("post-newer: token after cutoff should NOT be revoked")
	}
}

// TestRevokeUserZeroTTLFallsBackToDefault confirms the safety net
// against a caller accidentally passing ttl=0, which would otherwise
// SET the key without an EXPIRE and leak it forever.
func TestRevokeUserZeroTTLFallsBackToDefault(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	now := time.Now().UTC()

	if err := store.RevokeUser(ctx, wsID, userID, now, 0); err != nil {
		t.Fatalf("revoke-user: %v", err)
	}
	ttl := mr.TTL(userRevokedKey(wsID, userID))
	if ttl <= 0 {
		t.Fatalf("expected positive TTL on cutoff key, got %v", ttl)
	}
	// The default is 24h; allow some slack because miniredis
	// returns the remaining TTL at observation time.
	if ttl < 23*time.Hour {
		t.Errorf("default TTL too short: got %v, want >= 23h", ttl)
	}
}

func TestSessionTTL(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	sessionID := uuid.NewString()

	if err := store.Set(ctx, sessionID, userID, wsID, 30*time.Second); err != nil {
		t.Fatalf("set: %v", err)
	}
	// FastForward past the TTL — miniredis exposes a clock hook so we
	// don't have to actually sleep.
	mr.FastForward(time.Minute)
	if _, _, err := store.Get(ctx, wsID, sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected expired session to be missing: %v", err)
	}
}
