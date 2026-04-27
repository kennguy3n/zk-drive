package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newTestRedis returns a fresh in-process miniredis and a connected
// client. The client is closed and the server stopped via t.Cleanup.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return mr, c
}

// authedRequest fakes a request that has already been processed by
// the auth middleware so the rate limiter sees a populated user /
// workspace context.
func authedRequest(userID, workspaceID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), userIDContextKey, userID)
	ctx = context.WithValue(ctx, workspaceIDContextKey, workspaceID)
	return req.WithContext(ctx)
}

// TestRateLimitAcrossReplicas simulates two API replicas pointed at
// the same Redis. Each replica only sees half of the requests, but
// the combined count must trip the limit because the counters are
// shared.
func TestRateLimitAcrossReplicas(t *testing.T) {
	_, client := newTestRedis(t)

	cfg := RedisRateLimiterConfig{PerUser: 4, PerWorkspace: 1000}
	replicaA := RedisRateLimiter(client, cfg)
	replicaB := RedisRateLimiter(client, cfg)

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
	// the per-user counter is shared.
	rec := httptest.NewRecorder()
	handlerB.ServeHTTP(rec, authedRequest(userID, workspaceID))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once combined budget exhausted, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("Retry-After header should be set on 429")
	}
}

// TestRateLimitFailsOpenOnRedisDown — if Redis is unavailable the
// middleware must allow the request rather than 429-ing every
// caller. We close the client to simulate a connectivity failure.
func TestRateLimitFailsOpenOnRedisDown(t *testing.T) {
	mr, client := newTestRedis(t)
	mr.Close() // mimic Redis going away while the server keeps running.

	mw := RedisRateLimiter(client, RedisRateLimiterConfig{PerUser: 1, PerWorkspace: 1})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(uuid.New(), uuid.New()))
	if !called {
		t.Fatalf("handler should be invoked when Redis is down (fail-open)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (fail-open), got %d", rec.Code)
	}
}

// TestRateLimitWithoutUserID — anonymous traffic (no auth context)
// passes through unchanged, matching the in-memory implementation.
func TestRateLimitWithoutUserID(t *testing.T) {
	_, client := newTestRedis(t)
	mw := RedisRateLimiter(client, RedisRateLimiterConfig{PerUser: 1, PerWorkspace: 1})

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
