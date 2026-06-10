package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// reputationHandler is a fake auth endpoint whose status is driven by
// the test so the guard's record/reset behaviour can be exercised
// without a real credential check.
func reputationHandler(status *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if *status == http.StatusOK {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		RespondError(w, *status, ErrCodeAuthInvalidCredentials, "bad credentials")
	})
}

func newReputationTestGuard(t *testing.T) (*AuthReputation, func(http.Handler) http.Handler) {
	t.Helper()
	_, client := newTestRedis(t)
	rep := NewAuthReputation(client, AuthReputationConfig{
		FailureThreshold: 3,
		Delays:           []time.Duration{50 * time.Millisecond, 100 * time.Millisecond},
		BlockDuration:    200 * time.Millisecond,
		Retention:        time.Hour,
	}, 0)
	return rep, AuthReputationGuard(rep)
}

func doLogin(t *testing.T, guard func(http.Handler) http.Handler, h http.Handler, ip string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	guard(h).ServeHTTP(rec, req)
	return rec
}

// TestAuthReputationProgressiveCooldown verifies the failure threshold,
// the escalation through the configured delays, and that the cooldown
// rejects in-window attempts with 429 + Retry-After before the handler
// runs.
func TestAuthReputationProgressiveCooldown(t *testing.T) {
	_, guard := newReputationTestGuard(t)
	status := http.StatusUnauthorized
	h := reputationHandler(&status)
	const ip = "203.0.113.7"

	// First two failures are below threshold → 401, no cooldown.
	for i := 0; i < 2; i++ {
		rec := doLogin(t, guard, h, ip)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, rec.Code)
		}
	}

	// Third failure crosses the threshold and arms the first cooldown;
	// the response is still the handler's 401.
	rec := doLogin(t, guard, h, ip)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("threshold attempt: want 401, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After once the cooldown is armed")
	}

	// An immediate further attempt is throttled before the handler runs.
	rec = doLogin(t, guard, h, ip)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 during cooldown, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if got := decodeErrCode(t, rec.Body.Bytes()); got != ErrCodeAuthThrottled {
		t.Errorf("want %s, got %s", ErrCodeAuthThrottled, got)
	}
}

// TestAuthReputationResetOnSuccess confirms a 2xx clears the IP's
// reputation so a legitimate user is not punished for earlier typos.
func TestAuthReputationResetOnSuccess(t *testing.T) {
	_, guard := newReputationTestGuard(t)
	status := http.StatusUnauthorized
	h := reputationHandler(&status)
	const ip = "198.51.100.42"

	// Two failures (below threshold).
	for i := 0; i < 2; i++ {
		_ = doLogin(t, guard, h, ip)
	}

	// A success resets the counter.
	status = http.StatusOK
	if rec := doLogin(t, guard, h, ip); rec.Code != http.StatusOK {
		t.Fatalf("want 200 on success, got %d", rec.Code)
	}

	// Back to failing: the counter restarted, so the next two attempts
	// are again below threshold (401, never 429).
	status = http.StatusUnauthorized
	for i := 0; i < 2; i++ {
		rec := doLogin(t, guard, h, ip)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: want 401, got %d", i+1, rec.Code)
		}
	}
}

// TestAuthReputationIsolatesIPs ensures one noisy IP's cooldown does
// not bleed onto a different client IP.
func TestAuthReputationIsolatesIPs(t *testing.T) {
	_, guard := newReputationTestGuard(t)
	status := http.StatusUnauthorized
	h := reputationHandler(&status)

	// Drive the noisy IP into cooldown (3 failures arms it, 4th is 429).
	for i := 0; i < 3; i++ {
		_ = doLogin(t, guard, h, "203.0.113.9")
	}
	if rec := doLogin(t, guard, h, "203.0.113.9"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("noisy IP should be throttled, got %d", rec.Code)
	}

	// A different IP is unaffected.
	if rec := doLogin(t, guard, h, "203.0.113.250"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unrelated IP must not be throttled, got %d", rec.Code)
	}
}

// TestAuthReputationNilGuardPassThrough verifies the nil-guard returns
// a transparent pass-through so wiring stays branch-free when disabled.
func TestAuthReputationNilGuardPassThrough(t *testing.T) {
	guard := AuthReputationGuard(nil)
	status := http.StatusUnauthorized
	h := reputationHandler(&status)
	for i := 0; i < 10; i++ {
		if rec := doLogin(t, guard, h, "203.0.113.1"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("nil guard must pass through unchanged, got %d", rec.Code)
		}
	}
}

// TestAuthReputationInMemoryFallback exercises the no-Redis path so a
// single-process deployment still gets brute-force protection.
func TestAuthReputationInMemoryFallback(t *testing.T) {
	rep := NewAuthReputation(nil, AuthReputationConfig{
		FailureThreshold: 2,
		Delays:           []time.Duration{50 * time.Millisecond},
		BlockDuration:    100 * time.Millisecond,
		Retention:        time.Hour,
	}, 0)
	guard := AuthReputationGuard(rep)
	status := http.StatusUnauthorized
	h := reputationHandler(&status)
	const ip = "192.0.2.5"

	// Threshold 2: first failure free, second arms cooldown.
	_ = doLogin(t, guard, h, ip)
	_ = doLogin(t, guard, h, ip)
	if rec := doLogin(t, guard, h, ip); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("in-memory guard should throttle, got %d", rec.Code)
	}
}

func decodeErrCode(t *testing.T, body []byte) ErrorCode {
	t.Helper()
	var er ErrorResponse
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, string(body))
	}
	return er.Code
}
