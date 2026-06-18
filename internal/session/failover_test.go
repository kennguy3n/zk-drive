package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// flakyStore is a Store whose every method returns failErr when fail
// is set. It records how many calls it served so tests can assert
// whether the primary or fallback handled a request.
type flakyStore struct {
	mu      sync.Mutex
	fail    error
	calls   int
	backing *MemoryStore
}

func newFlakyStore() *flakyStore {
	return &flakyStore{backing: NewMemoryStore()}
}

func (f *flakyStore) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *flakyStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *flakyStore) gate() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.fail
}

func (f *flakyStore) Set(ctx context.Context, sid string, uid, ws uuid.UUID, ttl time.Duration) error {
	if err := f.gate(); err != nil {
		return err
	}
	return f.backing.Set(ctx, sid, uid, ws, ttl)
}
func (f *flakyStore) Get(ctx context.Context, ws uuid.UUID, sid string) (uuid.UUID, uuid.UUID, error) {
	if err := f.gate(); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return f.backing.Get(ctx, ws, sid)
}
func (f *flakyStore) Revoke(ctx context.Context, ws uuid.UUID, sid string) error {
	if err := f.gate(); err != nil {
		return err
	}
	return f.backing.Revoke(ctx, ws, sid)
}
func (f *flakyStore) RevokeAllForUser(ctx context.Context, ws, uid uuid.UUID) error {
	if err := f.gate(); err != nil {
		return err
	}
	return f.backing.RevokeAllForUser(ctx, ws, uid)
}
func (f *flakyStore) RevokeUser(ctx context.Context, ws, uid uuid.UUID, at time.Time, ttl time.Duration) error {
	if err := f.gate(); err != nil {
		return err
	}
	return f.backing.RevokeUser(ctx, ws, uid, at, ttl)
}
func (f *flakyStore) IsRevoked(ctx context.Context, ws, uid uuid.UUID, issuedAt time.Time) (bool, error) {
	if err := f.gate(); err != nil {
		return false, err
	}
	return f.backing.IsRevoked(ctx, ws, uid, issuedAt)
}
func (f *flakyStore) Create(ctx context.Context, rec SessionRecord, ttl time.Duration) error {
	if err := f.gate(); err != nil {
		return err
	}
	return f.backing.Create(ctx, rec, ttl)
}
func (f *flakyStore) GetRecord(ctx context.Context, ws uuid.UUID, sid string) (SessionRecord, error) {
	if err := f.gate(); err != nil {
		return SessionRecord{}, err
	}
	return f.backing.GetRecord(ctx, ws, sid)
}
func (f *flakyStore) ListForUser(ctx context.Context, ws, uid uuid.UUID) ([]SessionRecord, error) {
	if err := f.gate(); err != nil {
		return nil, err
	}
	return f.backing.ListForUser(ctx, ws, uid)
}
func (f *flakyStore) RevokeForUser(ctx context.Context, ws, uid uuid.UUID, sid string) (bool, error) {
	if err := f.gate(); err != nil {
		return false, err
	}
	return f.backing.RevokeForUser(ctx, ws, uid, sid)
}
func (f *flakyStore) ValidateSession(ctx context.Context, ws uuid.UUID, sid, userAgent, clientIP string) error {
	if err := f.gate(); err != nil {
		return err
	}
	return f.backing.ValidateSession(ctx, ws, sid, userAgent, clientIP)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"redis.Nil is a logical miss", redis.Nil, false},
		{"caller cancel", context.Canceled, false},
		{"caller deadline", context.DeadlineExceeded, false},
		{"net error", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"pool timeout", redis.ErrPoolTimeout, true},
		{"closed client", redis.ErrClosed, true},
		{"bare io.EOF (redis closed conn mid-command)", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped io.EOF", fmt.Errorf("read tcp: %w", io.EOF), true},
		{"logical error", errors.New("WRONGTYPE"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUnavailable(c.err); got != c.want {
				t.Fatalf("isUnavailable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestFailoverRoutesToPrimaryWhenHealthy(t *testing.T) {
	primary, fallback := newFlakyStore(), newFlakyStore()
	f := NewFailoverStore(primary, fallback, func(context.Context) error { return nil }, quietLogger())

	ws, uid := uuid.New(), uuid.New()
	if err := f.Set(context.Background(), "s", uid, ws, time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if primary.count() != 1 || fallback.count() != 0 {
		t.Fatalf("healthy store should use primary only (primary=%d fallback=%d)", primary.count(), fallback.count())
	}
	if !f.Healthy() {
		t.Fatal("store should still be healthy")
	}
}

func TestFailoverDegradesOnConnectivityError(t *testing.T) {
	primary, fallback := newFlakyStore(), newFlakyStore()
	f := NewFailoverStore(primary, fallback, func(context.Context) error { return nil }, quietLogger())

	// Simulate Redis going away with a net error.
	primary.setFail(&net.OpError{Op: "write", Err: errors.New("broken pipe")})

	ws, uid := uuid.New(), uuid.New()
	if err := f.Set(context.Background(), "s", uid, ws, time.Hour); err != nil {
		t.Fatalf("Set should transparently fall back, got %v", err)
	}
	if f.Healthy() {
		t.Fatal("store must be marked degraded after a connectivity error")
	}
	if fallback.count() != 1 {
		t.Fatalf("fallback should have served the request, count=%d", fallback.count())
	}

	// Subsequent calls must skip the primary entirely while degraded.
	primaryBefore := primary.count()
	_, _, _ = f.Get(context.Background(), ws, "s")
	if primary.count() != primaryBefore {
		t.Fatalf("degraded store must not touch primary (before=%d after=%d)", primaryBefore, primary.count())
	}
}

func TestFailoverPropagatesLogicalErrors(t *testing.T) {
	primary, fallback := newFlakyStore(), newFlakyStore()
	f := NewFailoverStore(primary, fallback, func(context.Context) error { return nil }, quietLogger())

	// A non-connectivity (logical) error must surface to the caller
	// and NOT trigger failover.
	logical := errors.New("WRONGTYPE Operation against a key")
	primary.setFail(logical)

	ws, uid := uuid.New(), uuid.New()
	err := f.Set(context.Background(), "s", uid, ws, time.Hour)
	if !errors.Is(err, logical) {
		t.Fatalf("logical error should propagate, got %v", err)
	}
	if !f.Healthy() {
		t.Fatal("logical error must NOT degrade the store")
	}
	if fallback.count() != 0 {
		t.Fatal("logical error must not hit the fallback")
	}
}

func TestFailoverHealthLoopRecovers(t *testing.T) {
	primary, fallback := newFlakyStore(), newFlakyStore()
	var mu sync.Mutex
	ready := false
	ping := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if ready {
			return nil
		}
		return &net.OpError{Op: "dial", Err: errors.New("refused")}
	}
	f := NewFailoverStore(primary, fallback, ping, quietLogger())
	f.pingTimeout = 100 * time.Millisecond

	// Force degraded.
	primary.setFail(&net.OpError{Op: "write", Err: errors.New("broken pipe")})
	_ = f.Set(context.Background(), "s", uuid.New(), uuid.New(), time.Hour)
	if f.Healthy() {
		t.Fatal("precondition: store should be degraded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.RunHealthLoop(ctx, 10*time.Millisecond)

	// Let Redis "recover".
	primary.setFail(nil)
	mu.Lock()
	ready = true
	mu.Unlock()

	deadline := time.After(2 * time.Second)
	for !f.Healthy() {
		select {
		case <-deadline:
			t.Fatal("health loop did not recover within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestFailoverFlushesRevocationsOnRecovery is the security regression
// for the recovery-loss window: a force-sign-out recorded while Redis
// is down must survive the switch back to Redis. Before the
// flush-on-recovery fix the revocation lived only in the in-memory
// fallback, so once IsRevoked resumed consulting Redis the killed
// token came back to life.
func TestFailoverFlushesRevocationsOnRecovery(t *testing.T) {
	primary := newFlakyStore()
	fallback := NewMemoryStore()
	var mu sync.Mutex
	ready := false
	ping := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if ready {
			return nil
		}
		return &net.OpError{Op: "dial", Err: errors.New("refused")}
	}
	f := NewFailoverStore(primary, fallback, ping, quietLogger())
	f.pingTimeout = 100 * time.Millisecond

	ws, uid := uuid.New(), uuid.New()
	issuedAt := time.Now().UTC().Add(-time.Minute) // token predates the revocation
	cutoff := time.Now().UTC()

	// Redis goes down, then a force-sign-out is issued: it must land in
	// the fallback, and Redis (primary) must NOT yet know about it.
	primary.setFail(&net.OpError{Op: "write", Err: errors.New("broken pipe")})
	if err := f.RevokeUser(context.Background(), ws, uid, cutoff, time.Hour); err != nil {
		t.Fatalf("RevokeUser during outage: %v", err)
	}
	if f.Healthy() {
		t.Fatal("precondition: store should be degraded after a connectivity error")
	}
	primary.setFail(nil)
	if revoked, _ := primary.backing.IsRevoked(context.Background(), ws, uid, issuedAt); revoked {
		t.Fatal("precondition: primary should not yet hold the degraded-window revocation")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.RunHealthLoop(ctx, 10*time.Millisecond)

	mu.Lock()
	ready = true
	mu.Unlock()

	deadline := time.After(2 * time.Second)
	for !f.Healthy() {
		select {
		case <-deadline:
			t.Fatal("health loop did not recover within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// After recovery the revocation must be present in Redis: a request
	// routed to the (now healthy) primary still sees the user as
	// revoked. This is the core anti-resurrection guarantee.
	revoked, err := f.IsRevoked(context.Background(), ws, uid, issuedAt)
	if err != nil {
		t.Fatalf("IsRevoked after recovery: %v", err)
	}
	if !revoked {
		t.Fatal("revocation was lost on recovery: token resurrected after Redis came back")
	}
	if got, _ := primary.backing.IsRevoked(context.Background(), ws, uid, issuedAt); !got {
		t.Fatal("revocation was not flushed into the primary store on recovery")
	}
}

// TestFailoverValidateSessionDegradesOpenForUnknownSession is the
// availability regression for the device-aware session gate
// under a Redis outage. A token whose session lives only in
// Redis (created before the outage) must NOT be hard-401'd while
// degraded: doing so would turn a transient Redis blip into a
// fleet-wide forced re-login and contradict the IsRevoked hot path,
// which degrades OPEN on the same outage. A session the fallback DOES
// know (created during the outage) must still be fully validated,
// including the device-anomaly check, so an outage-created session
// stays device-bound.
func TestFailoverValidateSessionDegradesOpenForUnknownSession(t *testing.T) {
	primary := newFlakyStore()
	fallback := NewMemoryStore()
	f := NewFailoverStore(primary, fallback, func(context.Context) error { return nil }, quietLogger())

	ws, uid := uuid.New(), uuid.New()

	// Redis goes away mid-flight.
	primary.setFail(&net.OpError{Op: "write", Err: errors.New("broken pipe")})

	// A pre-outage session lives only in Redis, so the empty fallback
	// has never heard of it. While degraded it must be admitted rather
	// than 401'd — the JWT + per-user cutoff still gate the request.
	if err := f.ValidateSession(context.Background(), ws, "pre-outage-sid", "Mozilla/5.0", "203.0.113.7"); err != nil {
		t.Fatalf("degraded ValidateSession on an unknown (pre-outage) session must admit, got %v", err)
	}
	if f.Healthy() {
		t.Fatal("precondition: store should be degraded after the connectivity error")
	}

	// A session created DURING the outage lands in the fallback and must
	// stay device-bound even while degraded.
	const loginUA, loginIP = "Mozilla/5.0 (login device)", "198.51.100.10"
	rec := SessionRecord{
		SessionID:   "outage-sid",
		UserID:      uid,
		WorkspaceID: ws,
		UserAgent:   loginUA,
		IP:          loginIP,
		DeviceHash:  Fingerprint(loginUA, loginIP),
		CreatedAt:   time.Now().UTC(),
		LastSeenAt:  time.Now().UTC(),
	}
	if err := f.Create(context.Background(), rec, time.Hour); err != nil {
		t.Fatalf("Create during outage: %v", err)
	}
	// Same device → admitted.
	if err := f.ValidateSession(context.Background(), ws, "outage-sid", loginUA, loginIP); err != nil {
		t.Fatalf("degraded ValidateSession for the originating device must admit, got %v", err)
	}
	// Different device → anomaly still enforced even while degraded: the
	// softening applies ONLY to sessions the fallback does not know.
	if err := f.ValidateSession(context.Background(), ws, "outage-sid", "curl/8.0 (attacker)", "10.0.0.9"); !errors.Is(err, ErrSessionAnomaly) {
		t.Fatalf("degraded ValidateSession must still enforce device anomaly for an outage-created session, got %v", err)
	}
}

// TestFailoverValidateSessionEnforcesWhenHealthy guards the other half
// of the contract: while Redis is reachable, a genuinely revoked /
// unknown session (ErrSessionNotFound from the primary) must still
// 401. The degraded-window softening must never leak into the healthy
// path.
func TestFailoverValidateSessionEnforcesWhenHealthy(t *testing.T) {
	primary := newFlakyStore()
	fallback := NewMemoryStore()
	f := NewFailoverStore(primary, fallback, func(context.Context) error { return nil }, quietLogger())

	// Redis is healthy but the session does not exist (revoked/expired):
	// the primary MemoryStore returns ErrSessionNotFound, which must
	// surface so AuthMiddleware 401s.
	err := f.ValidateSession(context.Background(), uuid.New(), "ghost-sid", "Mozilla/5.0", "203.0.113.7")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("healthy ValidateSession must surface ErrSessionNotFound, got %v", err)
	}
	if !f.Healthy() {
		t.Fatal("a logical not-found must not degrade the store")
	}
}
