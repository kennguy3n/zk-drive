package session

import (
	"context"
	"errors"
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
