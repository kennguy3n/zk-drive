package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redis/go-redis/v9"
)

// newIPLimitHandler wraps a 200-returning handler in the per-IP limiter
// (in-memory backend, client=nil) at the given per-IP budget.
func newIPLimitHandler(perIP int) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return IPRateLimiter(context.Background(), nil, perIP, 0)(next)
}

func doGet(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/platform/jwt/rotate", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIPRateLimiter_ThrottlesPerIP(t *testing.T) {
	const perIP = 2
	// Token-bucket capacity is perIP * burstMultiplier; that many
	// requests in the same instant are allowed before the bucket empties.
	cap := perIP * burstMultiplier
	h := newIPLimitHandler(perIP)
	const ip = "203.0.113.7:5555"

	for i := 0; i < cap; i++ {
		if rec := doGet(h, ip); rec.Code != http.StatusOK {
			t.Fatalf("request %d within burst: got %d, want 200", i+1, rec.Code)
		}
	}

	rec := doGet(h, ip)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request past burst: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 response missing Retry-After header")
	}
}

func TestIPRateLimiter_IndependentPerIP(t *testing.T) {
	const perIP = 1
	cap := perIP * burstMultiplier
	h := newIPLimitHandler(perIP)

	// Drain the first IP's bucket entirely.
	for i := 0; i <= cap; i++ {
		doGet(h, "198.51.100.1:1111")
	}
	if rec := doGet(h, "198.51.100.1:1111"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first IP should be throttled: got %d", rec.Code)
	}

	// A different IP must be unaffected by the first IP's flood.
	if rec := doGet(h, "198.51.100.2:2222"); rec.Code != http.StatusOK {
		t.Fatalf("second IP should not be throttled: got %d", rec.Code)
	}
}

// TestIPRateLimiter_TypedNilRedisClientUsesMemory guards the typed-nil
// interface gotcha: a (*redis.Client)(nil) passed into the
// redis.UniversalClient param must engage the in-memory fallback rather
// than entering the Redis path and panicking on a nil-client Run().
func TestIPRateLimiter_TypedNilRedisClientUsesMemory(t *testing.T) {
	var typedNil *redis.Client // nil pointer, non-nil when boxed in the interface
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	const perIP = 1
	h := IPRateLimiter(context.Background(), typedNil, perIP, 0)(next)

	const ip = "192.0.2.50:4444"
	cap := perIP * burstMultiplier
	for i := 0; i < cap; i++ {
		if rec := doGet(h, ip); rec.Code != http.StatusOK {
			t.Fatalf("request %d within burst: got %d, want 200 (no panic, in-memory path)", i+1, rec.Code)
		}
	}
	if rec := doGet(h, ip); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("past burst: got %d, want 429 — in-memory limiter should be active", rec.Code)
	}
}

// TestIPRateLimiter_SpoofedXFFCannotDodge confirms a client cannot evade
// the limit by injecting fresh left-most X-Forwarded-For values: with a
// trusted-proxy depth of 1 the rightmost (proxy-appended) entry is the
// key, so spoofed left entries don't create new buckets.
func TestIPRateLimiter_SpoofedXFFCannotDodge(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := IPRateLimiter(context.Background(), nil, 1, 1)(next)

	send := func(spoof string) int {
		req := httptest.NewRequest(http.MethodGet, "/platform/jwt/rotate", nil)
		req.RemoteAddr = "10.0.0.9:9999"
		// Left entry is attacker-controlled; rightmost is the trusted
		// proxy's view of the real peer and is what depth=1 selects.
		req.Header.Set("X-Forwarded-For", spoof+", 203.0.113.42")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	cap := 1 * burstMultiplier
	for i := 0; i <= cap; i++ {
		send("1.1.1." + string(rune('0'+i)))
	}
	if code := send("9.9.9.9"); code != http.StatusTooManyRequests {
		t.Fatalf("rotating spoofed XFF should not dodge the limit: got %d, want 429", code)
	}
}
