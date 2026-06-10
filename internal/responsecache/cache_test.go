package responsecache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func newTestCache(t *testing.T) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb), mr
}

type payload struct {
	N int `json:"n"`
}

func TestGetOrComputeCachesAndServesHits(t *testing.T) {
	c, _ := newTestCache(t)
	ws := uuid.New()
	var calls atomic.Int64
	compute := func(context.Context) (payload, error) {
		calls.Add(1)
		return payload{N: 42}, nil
	}

	// MISS: computes and caches.
	got, err := GetOrCompute(context.Background(), c, ws, "folder", "k1", time.Minute, compute)
	if err != nil || got.N != 42 {
		t.Fatalf("first call: got %+v err %v", got, err)
	}
	// HIT: served from cache, compute not invoked again.
	got, err = GetOrCompute(context.Background(), c, ws, "folder", "k1", time.Minute, compute)
	if err != nil || got.N != 42 {
		t.Fatalf("second call: got %+v err %v", got, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("compute called %d times, want 1 (second should be a cache hit)", n)
	}
}

func TestBustWorkspaceInvalidates(t *testing.T) {
	c, _ := newTestCache(t)
	ws := uuid.New()
	var calls atomic.Int64
	compute := func(context.Context) (payload, error) {
		calls.Add(1)
		return payload{N: int(calls.Load())}, nil
	}

	if _, err := GetOrCompute(context.Background(), c, ws, "usage", "k", time.Minute, compute); err != nil {
		t.Fatal(err)
	}
	c.BustWorkspace(context.Background(), ws)
	// After a bust the generation advances, so the prior entry is
	// unreachable and compute runs again.
	got, err := GetOrCompute(context.Background(), c, ws, "usage", "k", time.Minute, compute)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || got.N != 2 {
		t.Fatalf("bust did not invalidate: calls=%d got=%+v", calls.Load(), got)
	}
}

func TestBustIsWorkspaceScoped(t *testing.T) {
	c, _ := newTestCache(t)
	wsA, wsB := uuid.New(), uuid.New()
	var aCalls, bCalls atomic.Int64
	computeA := func(context.Context) (payload, error) { aCalls.Add(1); return payload{N: 1}, nil }
	computeB := func(context.Context) (payload, error) { bCalls.Add(1); return payload{N: 2}, nil }

	for i := 0; i < 2; i++ {
		_, _ = GetOrCompute(context.Background(), c, wsA, "folder", "k", time.Minute, computeA)
		_, _ = GetOrCompute(context.Background(), c, wsB, "folder", "k", time.Minute, computeB)
	}
	// Busting A must not evict B's entry.
	c.BustWorkspace(context.Background(), wsA)
	_, _ = GetOrCompute(context.Background(), c, wsA, "folder", "k", time.Minute, computeA)
	_, _ = GetOrCompute(context.Background(), c, wsB, "folder", "k", time.Minute, computeB)

	if aCalls.Load() != 2 {
		t.Fatalf("A computed %d times, want 2 (initial + post-bust)", aCalls.Load())
	}
	if bCalls.Load() != 1 {
		t.Fatalf("B computed %d times, want 1 (bust of A must not touch B)", bCalls.Load())
	}
}

func TestTTLExpiry(t *testing.T) {
	c, mr := newTestCache(t)
	ws := uuid.New()
	var calls atomic.Int64
	compute := func(context.Context) (payload, error) { calls.Add(1); return payload{N: 7}, nil }

	if _, err := GetOrCompute(context.Background(), c, ws, "search", "q", 30*time.Second, compute); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(31 * time.Second)
	if _, err := GetOrCompute(context.Background(), c, ws, "search", "q", 30*time.Second, compute); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("TTL did not expire entry: compute calls=%d, want 2", calls.Load())
	}
}

func TestComputeErrorNotCached(t *testing.T) {
	c, _ := newTestCache(t)
	ws := uuid.New()
	wantErr := errors.New("boom")
	var calls atomic.Int64
	compute := func(context.Context) (payload, error) {
		calls.Add(1)
		return payload{}, wantErr
	}
	if _, err := GetOrCompute(context.Background(), c, ws, "folder", "k", time.Minute, compute); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	// A second call must recompute (the error was not cached).
	_, _ = GetOrCompute(context.Background(), c, ws, "folder", "k", time.Minute, compute)
	if calls.Load() != 2 {
		t.Fatalf("error was cached: compute calls=%d, want 2", calls.Load())
	}
}

func TestNilCacheIsPassThrough(t *testing.T) {
	var c *Cache // nil
	if c.Enabled() {
		t.Fatal("nil cache should report !Enabled")
	}
	var calls atomic.Int64
	compute := func(context.Context) (payload, error) { calls.Add(1); return payload{N: 5}, nil }
	got, err := GetOrCompute(context.Background(), c, uuid.New(), "folder", "k", time.Minute, compute)
	if err != nil || got.N != 5 {
		t.Fatalf("got %+v err %v", got, err)
	}
	// Bust on nil cache must be a no-op, not a panic.
	c.BustWorkspace(context.Background(), uuid.New())
	if calls.Load() != 1 {
		t.Fatalf("compute calls=%d, want 1", calls.Load())
	}
}

func TestFailOpenOnRedisOutage(t *testing.T) {
	c, mr := newTestCache(t)
	ws := uuid.New()
	mr.Close() // simulate Redis down
	var calls atomic.Int64
	compute := func(context.Context) (payload, error) { calls.Add(1); return payload{N: 9}, nil }
	got, err := GetOrCompute(context.Background(), c, ws, "folder", "k", time.Minute, compute)
	if err != nil || got.N != 9 {
		t.Fatalf("fail-open broke: got %+v err %v", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("compute calls=%d, want 1", calls.Load())
	}
}
