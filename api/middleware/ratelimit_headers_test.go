package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

// assertRateLimitHeaders checks the X-RateLimit-* triplet is present,
// well-formed, and self-consistent (0 <= remaining <= limit, reset in
// the future).
func assertRateLimitHeaders(t *testing.T, h http.Header) {
	t.Helper()
	limitStr := h.Get("X-RateLimit-Limit")
	remStr := h.Get("X-RateLimit-Remaining")
	resetStr := h.Get("X-RateLimit-Reset")
	if limitStr == "" || remStr == "" || resetStr == "" {
		t.Fatalf("missing rate-limit headers: limit=%q remaining=%q reset=%q", limitStr, remStr, resetStr)
	}
	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		t.Fatalf("bad limit header %q: %v", limitStr, err)
	}
	rem, err := strconv.Atoi(remStr)
	if err != nil {
		t.Fatalf("bad remaining header %q: %v", remStr, err)
	}
	if _, err := strconv.ParseInt(resetStr, 10, 64); err != nil {
		t.Fatalf("bad reset header %q: %v", resetStr, err)
	}
	if rem < 0 || rem > limit {
		t.Fatalf("remaining %d out of range [0,%d]", rem, limit)
	}
}

// TestRedisRateLimitHeadersOnAllow verifies the headers are emitted on
// an allowed response and that remaining decrements request over
// request.
func TestRedisRateLimitHeadersOnAllow(t *testing.T) {
	_, client := newTestRedis(t)
	mw := RedisRateLimiter(context.Background(), client, RedisRateLimiterConfig{PerUser: 5, PerWorkspace: 1000})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	user, ws := uuid.New(), uuid.New()
	var prev int = -1
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, authedRequest(user, ws))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
		assertRateLimitHeaders(t, rec.Header())
		rem, _ := strconv.Atoi(rec.Header().Get("X-RateLimit-Remaining"))
		if prev >= 0 && rem >= prev {
			t.Fatalf("remaining should decrease: prev=%d now=%d", prev, rem)
		}
		prev = rem
	}
}

// TestRedisRateLimitHeadersOnThrottle verifies the headers are present
// on the 429 too, with remaining pinned at 0.
func TestRedisRateLimitHeadersOnThrottle(t *testing.T) {
	_, client := newTestRedis(t)
	mw := RedisRateLimiter(context.Background(), client, RedisRateLimiterConfig{PerUser: 1, PerWorkspace: 1000})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	user, ws := uuid.New(), uuid.New()
	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		last = httptest.NewRecorder()
		handler.ServeHTTP(last, authedRequest(user, ws))
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after exhausting budget, got %d", last.Code)
	}
	assertRateLimitHeaders(t, last.Header())
	if rem := last.Header().Get("X-RateLimit-Remaining"); rem != "0" {
		t.Fatalf("throttled response should report 0 remaining, got %q", rem)
	}
}

// TestInMemoryRateLimitHeaders verifies the in-memory limiter sets the
// same headers as the Redis limiter and, crucially, reports the SAME
// X-RateLimit-Limit for the same configured PerUser rate — i.e. the
// configured steady-state rate, not the 2x burst capacity. This locks
// the two backends to one observable contract so a client never sees
// Limit flip (e.g. 5 -> 10) when the deployment swaps backing store.
func TestInMemoryRateLimitHeaders(t *testing.T) {
	const perUser = 5
	mw := RateLimiter(context.Background(), RateLimitConfig{PerUser: perUser, PerWorkspace: 1000})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(uuid.New(), uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	assertRateLimitHeaders(t, rec.Header())
	if got := rec.Header().Get("X-RateLimit-Limit"); got != strconv.Itoa(perUser) {
		t.Fatalf("in-memory limiter should report the configured per-user rate %d as the limit (matching the Redis limiter), got %q", perUser, got)
	}
}

// TestRateLimitHeadersConsistentAcrossBackends asserts the in-memory
// and Redis limiters advertise an identical X-RateLimit-Limit for the
// same configured PerUser, so the header is backend-agnostic.
func TestRateLimitHeadersConsistentAcrossBackends(t *testing.T) {
	const perUser = 7
	user, ws := uuid.New(), uuid.New()

	memRec := httptest.NewRecorder()
	RateLimiter(context.Background(), RateLimitConfig{PerUser: perUser, PerWorkspace: 1000})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	).ServeHTTP(memRec, authedRequest(user, ws))

	_, client := newTestRedis(t)
	redisRec := httptest.NewRecorder()
	RedisRateLimiter(context.Background(), client, RedisRateLimiterConfig{PerUser: perUser, PerWorkspace: 1000})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	).ServeHTTP(redisRec, authedRequest(user, ws))

	memLimit := memRec.Header().Get("X-RateLimit-Limit")
	redisLimit := redisRec.Header().Get("X-RateLimit-Limit")
	if memLimit != redisLimit {
		t.Fatalf("X-RateLimit-Limit differs across backends: in-memory=%q redis=%q", memLimit, redisLimit)
	}
	if memLimit != strconv.Itoa(perUser) {
		t.Fatalf("both backends should report the configured per-user rate %d, got %q", perUser, memLimit)
	}
}
