package session

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemoryStoreSetGet(t *testing.T) {
	m := NewMemoryStore()
	ws, uid := uuid.New(), uuid.New()
	sid := "sess-1"

	if err := m.Set(context.Background(), sid, uid, ws, time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	gotUID, gotWS, err := m.Get(context.Background(), ws, sid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotUID != uid || gotWS != ws {
		t.Fatalf("Get returned (%s,%s), want (%s,%s)", gotUID, gotWS, uid, ws)
	}
}

func TestMemoryStoreGetUnknown(t *testing.T) {
	m := NewMemoryStore()
	if _, _, err := m.Get(context.Background(), uuid.New(), "nope"); err != ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
	if _, _, err := m.Get(context.Background(), uuid.New(), ""); err != ErrSessionNotFound {
		t.Fatalf("empty sid err = %v, want ErrSessionNotFound", err)
	}
}

func TestMemoryStoreExpiry(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	m := NewMemoryStore()
	m.now = clock.now

	ws, uid := uuid.New(), uuid.New()
	if err := m.Set(context.Background(), "s", uid, ws, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	clock.advance(2 * time.Minute)
	if _, _, err := m.Get(context.Background(), ws, "s"); err != ErrSessionNotFound {
		t.Fatalf("expired session err = %v, want ErrSessionNotFound", err)
	}
}

func TestMemoryStoreRevoke(t *testing.T) {
	m := NewMemoryStore()
	ws, uid := uuid.New(), uuid.New()
	_ = m.Set(context.Background(), "s", uid, ws, time.Hour)
	if err := m.Revoke(context.Background(), ws, "s"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, _, err := m.Get(context.Background(), ws, "s"); err != ErrSessionNotFound {
		t.Fatalf("revoked session still present: %v", err)
	}
}

func TestMemoryStoreRevokeAllForUser(t *testing.T) {
	m := NewMemoryStore()
	ws, uid := uuid.New(), uuid.New()
	_ = m.Set(context.Background(), "s1", uid, ws, time.Hour)
	_ = m.Set(context.Background(), "s2", uid, ws, time.Hour)
	// A session for a different user must survive.
	other := uuid.New()
	_ = m.Set(context.Background(), "s3", other, ws, time.Hour)

	if err := m.RevokeAllForUser(context.Background(), ws, uid); err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}
	for _, sid := range []string{"s1", "s2"} {
		if _, _, err := m.Get(context.Background(), ws, sid); err != ErrSessionNotFound {
			t.Fatalf("%s should be revoked, got %v", sid, err)
		}
	}
	if _, _, err := m.Get(context.Background(), ws, "s3"); err != nil {
		t.Fatalf("other user's session should survive, got %v", err)
	}
}

func TestMemoryStoreRevokeUserCutoff(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0).UTC()}
	m := NewMemoryStore()
	m.now = clock.now
	ws, uid := uuid.New(), uuid.New()

	issuedOld := clock.t.Add(-time.Hour)
	issuedNew := clock.t.Add(time.Hour)

	// Revoke everything issued at/before now.
	if err := m.RevokeUser(context.Background(), ws, uid, clock.t, time.Hour); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	revoked, err := m.IsRevoked(context.Background(), ws, uid, issuedOld)
	if err != nil || !revoked {
		t.Fatalf("token issued before cutoff should be revoked (revoked=%v err=%v)", revoked, err)
	}
	revoked, err = m.IsRevoked(context.Background(), ws, uid, issuedNew)
	if err != nil || revoked {
		t.Fatalf("token issued after cutoff must remain valid (revoked=%v err=%v)", revoked, err)
	}
}

func TestMemoryStoreRevokeUserMonotonic(t *testing.T) {
	m := NewMemoryStore()
	ws, uid := uuid.New(), uuid.New()
	t1 := time.Unix(2_000_000, 0).UTC()
	t0 := t1.Add(-time.Hour)

	_ = m.RevokeUser(context.Background(), ws, uid, t1, time.Hour)
	// An out-of-order, earlier revocation must NOT move the cutoff back.
	_ = m.RevokeUser(context.Background(), ws, uid, t0, time.Hour)

	// A token issued between t0 and t1 must stay revoked.
	mid := t0.Add(30 * time.Minute)
	revoked, err := m.IsRevoked(context.Background(), ws, uid, mid)
	if err != nil || !revoked {
		t.Fatalf("cutoff regressed: token between t0 and t1 should be revoked (revoked=%v err=%v)", revoked, err)
	}
}

func TestMemoryStoreReapEvictsExpired(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	m := NewMemoryStore()
	m.now = clock.now
	ws, uid := uuid.New(), uuid.New()
	_ = m.Set(context.Background(), "s", uid, ws, time.Minute)
	_ = m.RevokeUser(context.Background(), ws, uid, clock.t, time.Minute)

	clock.advance(2 * time.Minute)
	m.reap()

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 0 || len(m.userIndex) != 0 || len(m.revoked) != 0 {
		t.Fatalf("reap left entries: sessions=%d userIndex=%d revoked=%d",
			len(m.sessions), len(m.userIndex), len(m.revoked))
	}
}

// fakeClock is a controllable time source for deterministic expiry tests.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
