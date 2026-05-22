package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubChecker is a deterministic Checker used to drive ReadyHandler
// branches in tests without bringing up real Postgres / Redis / S3 /
// NATS dependencies.
type stubChecker struct {
	name string
	err  error
	// delay simulates a slow dependency so the timeout branch can
	// be exercised. Zero means "return immediately".
	delay time.Duration
}

func (s *stubChecker) Name() string { return s.name }
func (s *stubChecker) Check(ctx context.Context) error {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

func TestReadyHandlerAllPass(t *testing.T) {
	svc := NewService([]Checker{
		&stubChecker{name: "postgres"},
		&stubChecker{name: "redis"},
		&stubChecker{name: "storage"},
		&stubChecker{name: "nats"},
	}, 500*time.Millisecond)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	svc.ReadyHandler()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp readyResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Status != "ready" {
		t.Fatalf("expected status=ready, got %q", resp.Status)
	}
	for _, name := range []string{"postgres", "redis", "storage", "nats"} {
		if got := resp.Checks[name]; got != "ok" {
			t.Errorf("check %q: expected ok, got %q", name, got)
		}
	}
	// Cache-Control must pin no-cache/no-store so an intermediary
	// (Envoy sidecar, CDN, mesh PEP) can't serve a stale 200 over a
	// genuinely failing pod. Failing this assertion is what would
	// allow a real outage to be masked by an upstream cache.
	if got := rr.Header().Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
		t.Errorf("Cache-Control header: expected no-cache,no-store,must-revalidate; got %q", got)
	}
}

func TestReadyHandlerOneFailing(t *testing.T) {
	svc := NewService([]Checker{
		&stubChecker{name: "postgres"},
		&stubChecker{name: "redis", err: errors.New("ping: connection refused")},
		&stubChecker{name: "storage"},
	}, 500*time.Millisecond)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	svc.ReadyHandler()(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp readyResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Status != "not_ready" {
		t.Fatalf("expected status=not_ready, got %q", resp.Status)
	}
	if resp.Checks["postgres"] != "ok" {
		t.Errorf("postgres: expected ok, got %q", resp.Checks["postgres"])
	}
	// Response surfaces only the checker name + generic "fail"; the
	// underlying cause ("connection refused") MUST NOT appear in
	// the response body — it's emitted to the server log instead so
	// /readyz can't be used to leak internal network topology.
	if got := resp.Checks["redis"]; got != "fail" {
		t.Errorf("redis: expected generic fail status, got %q", got)
	}
	if strings.Contains(rr.Body.String(), "connection refused") {
		t.Errorf("response body leaked underlying error: %s", rr.Body.String())
	}
	if resp.Checks["storage"] != "ok" {
		t.Errorf("storage: expected ok, got %q", resp.Checks["storage"])
	}
}

func TestReadyHandlerNoCheckersDegenerate(t *testing.T) {
	svc := NewService(nil, 0)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	svc.ReadyHandler()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty-checker degenerate Service, got %d", rr.Code)
	}
	var resp readyResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Status != "ready" {
		t.Fatalf("expected status=ready, got %q", resp.Status)
	}
	if len(resp.Checks) != 0 {
		t.Fatalf("expected empty checks map, got %v", resp.Checks)
	}
}

func TestReadyHandlerCheckerTimeout(t *testing.T) {
	svc := NewService([]Checker{
		&stubChecker{name: "fast"},
		&stubChecker{name: "slow", delay: 500 * time.Millisecond},
	}, 50*time.Millisecond)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	start := time.Now()
	svc.ReadyHandler()(rr, req)
	elapsed := time.Since(start)

	// The slow checker should be cancelled by the per-check timeout,
	// not the response should block on it.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("response took %s, expected <250ms (timeout should cancel slow checker)", elapsed)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 due to slow checker timeout, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp readyResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Checks["fast"] != "ok" {
		t.Errorf("fast: expected ok, got %q", resp.Checks["fast"])
	}
	if got := resp.Checks["slow"]; got != "fail" {
		t.Errorf("slow: expected generic fail status (verbatim error must stay out of the body), got %q", got)
	}
}

func TestReadyHandlerRunsCheckersConcurrently(t *testing.T) {
	// Two 100ms checkers should complete in roughly 100ms total, not 200ms,
	// if ReadyHandler invokes them in parallel.
	svc := NewService([]Checker{
		&stubChecker{name: "a", delay: 100 * time.Millisecond},
		&stubChecker{name: "b", delay: 100 * time.Millisecond},
	}, 500*time.Millisecond)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	start := time.Now()
	svc.ReadyHandler()(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// Allow generous slack to account for scheduling jitter on CI,
	// but reject a clearly-sequential >180ms run.
	if elapsed > 180*time.Millisecond {
		t.Fatalf("checkers appear to run sequentially: total %s (each 100ms)", elapsed)
	}
}

// TestNewCheckerFunc verifies the closure-adapter passes the same
// context through to the wrapped function.
func TestNewCheckerFunc(t *testing.T) {
	var calls int32
	c := NewCheckerFunc("custom", func(_ context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if c.Name() != "custom" {
		t.Fatalf("Name(): expected custom, got %q", c.Name())
	}
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check(): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
}

// TestDefaultTimeoutAppliedWhenZero verifies the constructor's
// timeout floor behaviour. A zero or negative timeout should fall
// back to DefaultCheckTimeout rather than zero (which would
// instantly cancel every check).
func TestDefaultTimeoutAppliedWhenZero(t *testing.T) {
	svc := NewService([]Checker{&stubChecker{name: "ok"}}, 0)
	if svc.timeout != DefaultCheckTimeout {
		t.Fatalf("expected timeout=%s, got %s", DefaultCheckTimeout, svc.timeout)
	}
	svc = NewService([]Checker{&stubChecker{name: "ok"}}, -1*time.Second)
	if svc.timeout != DefaultCheckTimeout {
		t.Fatalf("negative timeout: expected DefaultCheckTimeout, got %s", svc.timeout)
	}
}
