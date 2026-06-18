package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/tenantctx"
)

// newTestRedis returns a fresh in-process miniredis and a connected
// client. The helper guarantees the returned client is responsive
// before returning — on heavily-loaded CI runners (where this test
// previously flaked under -race) miniredis.Run() can return before
// the kernel's accept queue is hot, causing the first command from
// go-redis's pool to fail with "connection refused after 5
// attempts" and trip the fail-open branch. A short PING wait
// converts that flake into a deterministic test setup.
// The client is closed and the server stopped via t.Cleanup.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	// Wait until the client can actually reach the listener. This
	// is a defence against listener / accept-queue races on slow
	// CI runners — Run() only guarantees net.Listen() succeeded,
	// not that the accept goroutine is scheduled.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := c.Ping(ctx).Err()
		cancel()
		if err == nil {
			return mr, c
		}
		if time.Now().After(deadline) {
			t.Fatalf("miniredis ping never succeeded: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// authedRequest fakes a request that has already been processed by
// the auth middleware so the rate limiter sees a populated user /
// workspace context.
func authedRequest(userID, workspaceID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), userIDContextKey, userID)
	ctx = tenantctx.WithWorkspaceID(ctx, workspaceID)
	return req.WithContext(ctx)
}

// TestRateLimitAcrossReplicas simulates two API replicas pointed at
// the same Redis. Each replica only sees half of the requests, but
// the combined count must trip the limit because the counters are
// shared.
func TestRateLimitAcrossReplicas(t *testing.T) {
	_, client := newTestRedis(t)

	cfg := RedisRateLimiterConfig{PerUser: 4, PerWorkspace: 1000}
	replicaA := RedisRateLimiter(context.Background(), client, cfg)
	replicaB := RedisRateLimiter(context.Background(), client, cfg)

	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handlerA := replicaA(noop)
	handlerB := replicaB(noop)

	userID := uuid.New()
	workspaceID := uuid.New()

	// 4 requests within budget — alternated across the two replicas.
	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		h := handlerA
		if i%2 == 1 {
			h = handlerB
		}
		h.ServeHTTP(rec, authedRequest(userID, workspaceID))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d expected 200, got %d", i, rec.Code)
		}
	}

	// 5th request through *either* replica must be limited because
	// the per-user counter is shared. On heavily-contended CI
	// runners miniredis's accept goroutine can briefly stall
	// between successive requests, causing the rate-limit script
	// to hit go-redis's 5-dial-retry budget and trip the
	// fail-open branch — which surfaces here as a 200 instead of
	// the expected 429. Distinguish that environmental flake from
	// a genuine limiter regression by pinging Redis on a 200 and
	// only failing the test when Redis is reachable (i.e. the
	// limiter is actually broken). When Redis is briefly
	// unreachable we wait for the listener to come back and re-
	// issue the request: the counter is still at 4 server-side
	// (the failed Eval was a network-level error, not a script
	// success), so the retry exercises the same invariant.
	rec := httptest.NewRecorder()
	handlerB.ServeHTTP(rec, authedRequest(userID, workspaceID))
	if rec.Code != http.StatusTooManyRequests {
		// Probe Redis. If it answers we have a real bug; if not,
		// wait briefly and retry the assertion once.
		probeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		probeErr := client.Ping(probeCtx).Err()
		cancel()
		if probeErr == nil {
			t.Fatalf("expected 429 once combined budget exhausted, got %d", rec.Code)
		}
		// Wait for the listener to recover, then retry once.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			ctx, c := context.WithTimeout(context.Background(), 200*time.Millisecond)
			err := client.Ping(ctx).Err()
			c()
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		rec = httptest.NewRecorder()
		handlerB.ServeHTTP(rec, authedRequest(userID, workspaceID))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 on retry after miniredis listener recovery, got %d", rec.Code)
		}
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("Retry-After header should be set on 429")
	}
}

// TestRateLimitFallsBackToMemoryOnRedisDown — when Redis is
// unreachable the middleware must NOT 429 every caller; it falls back
// to the per-replica in-memory token bucket. A first request
// from a fresh user/workspace therefore still passes. This is a
// fallback-to-local-limiting behaviour, NOT unlimited fail-open: the
// in-memory bucket still enforces a budget (covered by the in-memory
// limiter tests in ratelimit_test.go). We close the client to simulate
// a connectivity failure.
func TestRateLimitFallsBackToMemoryOnRedisDown(t *testing.T) {
	mr, client := newTestRedis(t)
	mr.Close() // mimic Redis going away while the server keeps running.

	mw := RedisRateLimiter(context.Background(), client, RedisRateLimiterConfig{PerUser: 1, PerWorkspace: 1})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(uuid.New(), uuid.New()))
	if !called {
		t.Fatalf("handler should be invoked when Redis is down (in-memory fallback)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from in-memory fallback, got %d", rec.Code)
	}
}

// TestUserDeniedDoesNotPollutWorkspaceCounter pins the per-user vs.
// per-workspace counter isolation: a user that exceeds
// their personal budget and keeps hammering must NOT inflate the
// workspace counter, otherwise a single misbehaving client can rate
// limit every other user in the workspace.
func TestUserDeniedDoesNotPollutWorkspaceCounter(t *testing.T) {
	_, client := newTestRedis(t)

	// User budget of 2, workspace budget of 5. Attacker sends 20
	// requests; the workspace counter must stay at 2 (the two
	// allowed) — every denied request must be a no-op for the
	// workspace counter.
	cfg := RedisRateLimiterConfig{PerUser: 2, PerWorkspace: 5}
	mw := RedisRateLimiter(context.Background(), client, cfg)
	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(noop)

	attacker := uuid.New()
	workspaceID := uuid.New()
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, authedRequest(attacker, workspaceID))
		// Past the user budget every request must be 429.
		if i >= 2 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("attacker request %d: expected 429, got %d", i, rec.Code)
		}
	}

	// A different user in the same workspace should still have
	// their full personal budget available — workspace counter
	// should be at 2 (only the attacker's two allowed requests
	// touched it), so 2 more requests fit comfortably.
	victim := uuid.New()
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, authedRequest(victim, workspaceID))
		if rec.Code != http.StatusOK {
			t.Fatalf("victim request %d: expected 200, got %d (attacker should not have starved the workspace)", i, rec.Code)
		}
	}
}

// TestRateLimitWithoutUserID — anonymous traffic (no auth context)
// passes through unchanged, matching the in-memory implementation.
func TestRateLimitWithoutUserID(t *testing.T) {
	_, client := newTestRedis(t)
	mw := RedisRateLimiter(context.Background(), client, RedisRateLimiterConfig{PerUser: 1, PerWorkspace: 1})

	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatalf("anonymous request should bypass the limiter")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
