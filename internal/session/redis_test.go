package session

import (
	"context"
	"errors"
	"fmt"
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

// TestUserSessionsIndexTTLNeverShrinks pins the contract that a
// short-lived session must
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
// powers per-user JWT revocation. The IsRevoked decision boundary is
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

// TestRevokeUserFloorsSubSecondTTL pins the sub-second TTL floor.
// Redis EX is in whole seconds and rejects 0 as an invalid expire
// time, so int64(ttl.Seconds()) truncating a (0, 1s) duration to 0
// would have surfaced as an EVAL error and the revocation would
// have silently been lost. We floor to 1s instead so any positive
// TTL produces a working revocation key.
func TestRevokeUserFloorsSubSecondTTL(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	at := time.Now().UTC()

	if err := store.RevokeUser(ctx, wsID, userID, at, 100*time.Millisecond); err != nil {
		t.Fatalf("revoke with sub-second TTL: %v", err)
	}

	// Within the 1-second floor window the key must be readable and
	// the cutoff must report the token as revoked.
	revoked, err := store.IsRevoked(ctx, wsID, userID, at)
	if err != nil {
		t.Fatalf("is-revoked: %v", err)
	}
	if !revoked {
		t.Fatal("token issued at cutoff must be considered revoked")
	}

	// And the underlying key must have an actual TTL (>= 1s, not 0).
	key := fmt.Sprintf("ws:%s:user_revoked:%s", wsID.String(), userID.String())
	ttl := mr.TTL(key)
	if ttl < time.Second {
		t.Fatalf("expected TTL floored to >= 1s, got %v", ttl)
	}
}

// TestIsRevokedRejectsMalformedCutoff pins the strict-parse path:
// a manually-corrupted cutoff (manual Redis surgery, an old
// pre-script value format, or memory corruption) must surface as
// an error from IsRevoked so the middleware fails closed, rather
// than silently parsing the leading digits and using a half-right
// number. fmt.Sscanf would tolerate "1700000000abc" → 1700000000;
// strconv.ParseInt does not.
func TestIsRevokedRejectsMalformedCutoff(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	userID := uuid.New()
	wsID := uuid.New()
	key := fmt.Sprintf("ws:%s:user_revoked:%s", wsID.String(), userID.String())

	cases := []struct {
		name string
		raw  string
	}{
		{"trailing letters", "1700000000abc"},
		{"fractional second", "1700000000.5"},
		{"hex prefix", "0x1234"},
		{"pure garbage", "not-a-number"},
		{"empty string", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := mr.Set(key, c.raw); err != nil {
				t.Fatalf("seed corrupt value: %v", err)
			}
			revoked, err := store.IsRevoked(ctx, wsID, userID, time.Now())
			if err == nil {
				t.Fatalf("malformed cutoff %q parsed without error (revoked=%v)", c.raw, revoked)
			}
			if revoked {
				t.Errorf("malformed cutoff %q: must not report revoked=true on parse failure", c.raw)
			}
		})
	}
}

// TestRevokeUserAtomicMaxUpdate is the regression test for the
// last-writer-wins race in atomic revocation updates: two
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

// --- 6.2 device-aware session tests ---

const (
	uaChrome  = "Mozilla/5.0 (X11; Linux x86_64) Chrome/120"
	uaFirefox = "Mozilla/5.0 (X11; Linux x86_64) Firefox/121"
)

// TestFingerprintStability pins the coarsening contract: same browser
// on a different host within the same /16 keeps a stable fingerprint
// (so a DHCP lease change doesn't log the user out), while a different
// browser OR a different /16 network changes it (so a replayed token
// from elsewhere is caught).
func TestFingerprintStability(t *testing.T) {
	base := Fingerprint(uaChrome, "203.0.113.10")
	cases := []struct {
		name     string
		ua       string
		ip       string
		wantSame bool
	}{
		{"identical", uaChrome, "203.0.113.10", true},
		{"same /16 different host", uaChrome, "203.0.200.55", true},
		{"different /16", uaChrome, "198.51.100.10", false},
		{"different UA same IP", uaFirefox, "203.0.113.10", false},
		{"empty ip stable", uaChrome, "", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := Fingerprint(c.ua, c.ip)
			if (got == base) != c.wantSame {
				t.Fatalf("Fingerprint(%q,%q)==base = %v, want %v", c.ua, c.ip, got == base, c.wantSame)
			}
		})
	}
}

// TestFingerprintIPv6Coarsening pins that IPv6 addresses are coarsened
// to the /48 routing prefix so addresses within the same allocation
// share a fingerprint while a different /48 does not.
func TestFingerprintIPv6Coarsening(t *testing.T) {
	a := Fingerprint(uaChrome, "2001:db8:1:abcd::1")
	if a != Fingerprint(uaChrome, "2001:db8:1:ffff::9") {
		t.Error("addresses in the same /48 must share a fingerprint")
	}
	if a == Fingerprint(uaChrome, "2001:db8:2:abcd::1") {
		t.Error("addresses in different /48 blocks must differ")
	}
}

// TestCreatePersistsDeviceInfoAndRefreshKeepsCreatedAt verifies the
// device columns round-trip through GetRecord and that re-Creating an
// existing session (the refresh path) preserves created_at while
// advancing last_seen — the contract that keeps "signed in at" stable
// across token refreshes.
func TestCreatePersistsDeviceInfoAndRefreshKeepsCreatedAt(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	userID, wsID := uuid.New(), uuid.New()
	sid := uuid.NewString()

	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	rec := SessionRecord{
		SessionID: sid, UserID: userID, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5",
		DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
		CreatedAt:  created, LastSeenAt: created,
	}
	if err := store.Create(ctx, rec, time.Hour); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.GetRecord(ctx, wsID, sid)
	if err != nil {
		t.Fatalf("get-record: %v", err)
	}
	if got.UserAgent != uaChrome || got.IP != "203.0.113.5" || got.DeviceHash != rec.DeviceHash {
		t.Fatalf("device info not persisted: %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at: got %v want %v", got.CreatedAt, created)
	}

	// Refresh: same sid, later last_seen, different created_at input —
	// created_at must NOT move (HSetNX), last_seen MUST advance.
	later := created.Add(30 * time.Minute)
	rec.CreatedAt = later
	rec.LastSeenAt = later
	if err := store.Create(ctx, rec, time.Hour); err != nil {
		t.Fatalf("refresh create: %v", err)
	}
	got, err = store.GetRecord(ctx, wsID, sid)
	if err != nil {
		t.Fatalf("get-record after refresh: %v", err)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at must survive refresh: got %v want %v", got.CreatedAt, created)
	}
	if !got.LastSeenAt.Equal(later) {
		t.Fatalf("last_seen must advance on refresh: got %v want %v", got.LastSeenAt, later)
	}
}

// TestListForUserSortsAndScopes verifies ListForUser returns only the
// target user's sessions, newest-activity first.
func TestListForUserSortsAndScopes(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	userID, wsID := uuid.New(), uuid.New()
	other := uuid.New()
	base := time.Now().UTC().Truncate(time.Second)

	mk := func(uid uuid.UUID, lastSeen time.Time) string {
		sid := uuid.NewString()
		if err := store.Create(ctx, SessionRecord{
			SessionID: sid, UserID: uid, WorkspaceID: wsID,
			UserAgent: uaChrome, IP: "203.0.113.5",
			DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
			CreatedAt:  base, LastSeenAt: lastSeen,
		}, time.Hour); err != nil {
			t.Fatalf("create: %v", err)
		}
		return sid
	}
	oldSid := mk(userID, base.Add(time.Minute))
	newSid := mk(userID, base.Add(time.Hour))
	mk(other, base.Add(2*time.Hour)) // must not appear

	recs, err := store.ListForUser(ctx, wsID, userID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 sessions for user, got %d", len(recs))
	}
	if recs[0].SessionID != newSid || recs[1].SessionID != oldSid {
		t.Fatalf("sessions not sorted newest-first: %s then %s", recs[0].SessionID, recs[1].SessionID)
	}
}

// TestListForUserPrunesStaleIndexEntries verifies that an index entry
// whose hash has expired is dropped from the result AND removed from
// the secondary index so it self-heals.
func TestListForUserPrunesStaleIndexEntries(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()
	userID, wsID := uuid.New(), uuid.New()

	shortSid := uuid.NewString()
	if err := store.Create(ctx, SessionRecord{
		SessionID: shortSid, UserID: userID, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5", DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
	}, time.Minute); err != nil {
		t.Fatalf("create short: %v", err)
	}
	longSid := uuid.NewString()
	if err := store.Create(ctx, SessionRecord{
		SessionID: longSid, UserID: userID, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5", DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
	}, time.Hour); err != nil {
		t.Fatalf("create long: %v", err)
	}

	mr.FastForward(5 * time.Minute) // short hash gone, index entry dangling

	recs, err := store.ListForUser(ctx, wsID, userID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 || recs[0].SessionID != longSid {
		t.Fatalf("expected only the long-lived session, got %+v", recs)
	}
	// Index must have been pruned: the SET now holds exactly one id.
	members, err := mr.Members(userSessionsKey(wsID, userID))
	if err != nil {
		t.Fatalf("set members: %v", err)
	}
	if len(members) != 1 || members[0] != longSid {
		t.Fatalf("stale index entry not pruned: members = %v", members)
	}
}

// TestRevokeForUserOwnershipScoped verifies a user can only revoke
// their own session and that an unknown id reports not-deleted.
func TestRevokeForUserOwnershipScoped(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	userID, wsID := uuid.New(), uuid.New()
	other := uuid.New()

	mine := uuid.NewString()
	if err := store.Create(ctx, SessionRecord{
		SessionID: mine, UserID: userID, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5", DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
	}, time.Hour); err != nil {
		t.Fatalf("create mine: %v", err)
	}
	theirs := uuid.NewString()
	if err := store.Create(ctx, SessionRecord{
		SessionID: theirs, UserID: other, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5", DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
	}, time.Hour); err != nil {
		t.Fatalf("create theirs: %v", err)
	}

	// Cannot revoke another user's session.
	deleted, err := store.RevokeForUser(ctx, wsID, userID, theirs)
	if err != nil {
		t.Fatalf("revoke theirs: %v", err)
	}
	if deleted {
		t.Fatal("must not revoke another user's session")
	}
	if _, err := store.GetRecord(ctx, wsID, theirs); err != nil {
		t.Fatalf("victim session should be intact: %v", err)
	}

	// Unknown id → not deleted, no error.
	if deleted, err := store.RevokeForUser(ctx, wsID, userID, uuid.NewString()); err != nil || deleted {
		t.Fatalf("unknown id: deleted=%v err=%v", deleted, err)
	}

	// Own session → deleted.
	deleted, err = store.RevokeForUser(ctx, wsID, userID, mine)
	if err != nil || !deleted {
		t.Fatalf("revoke mine: deleted=%v err=%v", deleted, err)
	}
	if _, err := store.GetRecord(ctx, wsID, mine); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("own session should be gone: %v", err)
	}
}

// TestValidateSession covers the per-request gate: device match passes,
// mismatch is an anomaly, a missing session is ErrSessionNotFound, and a
// legacy device-less session skips the anomaly check.
func TestValidateSession(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	userID, wsID := uuid.New(), uuid.New()

	sid := uuid.NewString()
	if err := store.Create(ctx, SessionRecord{
		SessionID: sid, UserID: userID, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5", DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
	}, time.Hour); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Same device (same /16) → OK.
	if err := store.ValidateSession(ctx, wsID, sid, uaChrome, "203.0.113.200"); err != nil {
		t.Fatalf("same-device validate: %v", err)
	}
	// Different network → anomaly.
	if err := store.ValidateSession(ctx, wsID, sid, uaChrome, "198.51.100.1"); !errors.Is(err, ErrSessionAnomaly) {
		t.Fatalf("expected anomaly on different network, got %v", err)
	}
	// Different UA → anomaly.
	if err := store.ValidateSession(ctx, wsID, sid, uaFirefox, "203.0.113.5"); !errors.Is(err, ErrSessionAnomaly) {
		t.Fatalf("expected anomaly on different UA, got %v", err)
	}
	// Unknown session → not found.
	if err := store.ValidateSession(ctx, wsID, uuid.NewString(), uaChrome, "203.0.113.5"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}

	// Legacy device-less session (written via Set) skips the anomaly
	// check but still enforces existence.
	legacy := uuid.NewString()
	if err := store.Set(ctx, legacy, userID, wsID, time.Hour); err != nil {
		t.Fatalf("set legacy: %v", err)
	}
	if err := store.ValidateSession(ctx, wsID, legacy, uaFirefox, "198.51.100.99"); err != nil {
		t.Fatalf("legacy session must skip anomaly: %v", err)
	}
}

// TestValidateSessionThrottlesLastSeen pins that last_seen is advanced
// at most once per throttle window so the hot path stays read-only.
// The throttle compares against the wall clock (not miniredis's TTL
// clock), so we drive it by seeding last_seen near vs. well before
// now rather than fast-forwarding.
func TestValidateSessionThrottlesLastSeen(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	userID, wsID := uuid.New(), uuid.New()

	// Case 1: recent last_seen → validate must NOT touch.
	fresh := uuid.NewString()
	recent := time.Now().UTC().Truncate(time.Second)
	if err := store.Create(ctx, SessionRecord{
		SessionID: fresh, UserID: userID, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5", DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
		CreatedAt: recent, LastSeenAt: recent,
	}, time.Hour); err != nil {
		t.Fatalf("create fresh: %v", err)
	}
	if err := store.ValidateSession(ctx, wsID, fresh, uaChrome, "203.0.113.5"); err != nil {
		t.Fatalf("validate fresh: %v", err)
	}
	got, _ := store.GetRecord(ctx, wsID, fresh)
	if !got.LastSeenAt.Equal(recent) {
		t.Fatalf("last_seen moved inside throttle window: %v != %v", got.LastSeenAt, recent)
	}

	// Case 2: stale last_seen (older than the throttle) → validate
	// advances it to ~now.
	stale := uuid.NewString()
	old := time.Now().UTC().Add(-2 * lastSeenThrottle).Truncate(time.Second)
	if err := store.Create(ctx, SessionRecord{
		SessionID: stale, UserID: userID, WorkspaceID: wsID,
		UserAgent: uaChrome, IP: "203.0.113.5", DeviceHash: Fingerprint(uaChrome, "203.0.113.5"),
		CreatedAt: old, LastSeenAt: old,
	}, time.Hour); err != nil {
		t.Fatalf("create stale: %v", err)
	}
	if err := store.ValidateSession(ctx, wsID, stale, uaChrome, "203.0.113.5"); err != nil {
		t.Fatalf("validate stale: %v", err)
	}
	got, _ = store.GetRecord(ctx, wsID, stale)
	if !got.LastSeenAt.After(old) {
		t.Fatalf("last_seen should advance past throttle window: got %v want > %v", got.LastSeenAt, old)
	}
}
