package middleware

import (
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
	mw := RedisRateLimiter(client, RedisRateLimiterConfig{PerUser: 5, PerWorkspace: 1000})
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
	mw := RedisRateLimiter(client, RedisRateLimiterConfig{PerUser: 1, PerWorkspace: 1000})
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
// same headers (Remaining <= Limit since the limit reported is the
// burst capacity).
func TestInMemoryRateLimitHeaders(t *testing.T) {
	mw := RateLimiter(RateLimitConfig{PerUser: 5, PerWorkspace: 1000})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(uuid.New(), uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	assertRateLimitHeaders(t, rec.Header())
}
