// Package health provides a deep readiness check endpoint that
// verifies the API server's dependencies (Postgres, Redis, storage,
// NATS JetStream) are reachable and responding before declaring the
// process "ready" to serve traffic.
//
// The package distinguishes two endpoints:
//
//   - /healthz (liveness): a shallow "the process is alive and the
//     HTTP server is accepting connections" check. Wired in
//     cmd/server/main.go without using this package. Should NEVER
//     ping downstream dependencies, because k8s interprets a failing
//     liveness probe as "restart the pod" — and restarting the pod
//     is the wrong response to a Redis outage.
//
//   - /readyz (readiness): the deep check this package implements.
//     k8s interprets a failing readiness probe as "stop sending
//     traffic to this pod until it recovers" — which IS the right
//     response to a Redis / Postgres / S3 outage.
//
// Each Checker is invoked under a per-check timeout shared from the
// Service so a slow dependency cannot pin the whole probe past the
// k8s probe timeout (default 1s). Errors are surfaced verbatim in
// the JSON response so operators can quickly identify which
// dependency is degraded.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// DefaultCheckTimeout is the per-check timeout applied by Service
// when no explicit timeout is configured. 2 seconds is comfortably
// below the default k8s readiness probe timeout (1s of HTTP timeout
// is generous for a dependency ping; the extra budget here covers
// goroutine scheduling overhead under load).
const DefaultCheckTimeout = 2 * time.Second

// Checker abstracts a single downstream-dependency probe. The Name
// is used as the JSON key in the readiness response so operators
// can spot which dependency failed; it should be short and stable
// (e.g. "postgres", "redis", "storage", "nats").
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckerFunc adapts a closure into a Checker without needing a
// dedicated struct per dependency. Use NewCheckerFunc to construct.
type CheckerFunc struct {
	name string
	fn   func(context.Context) error
}

// NewCheckerFunc wraps name + check function into a Checker.
func NewCheckerFunc(name string, fn func(context.Context) error) Checker {
	return &CheckerFunc{name: name, fn: fn}
}

// Name implements Checker.
func (c *CheckerFunc) Name() string { return c.name }

// Check implements Checker.
func (c *CheckerFunc) Check(ctx context.Context) error { return c.fn(ctx) }

// Service holds the registered checkers and exposes the readiness
// HTTP handler. Construct via NewService.
type Service struct {
	checkers []Checker
	timeout  time.Duration
}

// NewService constructs a Service with the supplied checkers and
// per-check timeout. A zero or negative timeout falls back to
// DefaultCheckTimeout. Pass an empty slice to construct a
// degenerate Service that always returns 200 — useful for tests.
func NewService(checkers []Checker, timeout time.Duration) *Service {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	return &Service{
		checkers: checkers,
		timeout:  timeout,
	}
}

// readyResponse is the JSON envelope for /readyz. Status is "ready"
// when all checks pass, "not_ready" otherwise. Checks maps each
// checker's Name() to either "ok" or "fail: <error message>".
type readyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// ReadyHandler returns an http.HandlerFunc that runs every registered
// checker concurrently with a per-check timeout, then writes a JSON
// envelope describing the result. Returns 200 when all checks pass
// and 503 when any check fails — which is what k8s expects for the
// "stop sending traffic" signal.
//
// Checkers are run in parallel so total wall time is bounded by the
// slowest dependency, not the sum. A 5-checker probe with a 2s
// timeout will return within ~2s of the first call regardless of
// how many backends are degraded.
func (s *Service) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		results := make(map[string]string, len(s.checkers))
		var (
			mu      sync.Mutex
			wg      sync.WaitGroup
			allOK   = true
		)
		// Each checker gets a fresh derived context with the per-
		// check timeout, but they all share the request context as
		// parent so a client disconnect cancels everything.
		for _, c := range s.checkers {
			wg.Add(1)
			go func(c Checker) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
				defer cancel()
				err := c.Check(ctx)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					results[c.Name()] = "fail: " + err.Error()
					allOK = false
				} else {
					results[c.Name()] = "ok"
				}
			}(c)
		}
		wg.Wait()

		status := http.StatusOK
		body := readyResponse{Status: "ready", Checks: results}
		if !allOK {
			status = http.StatusServiceUnavailable
			body.Status = "not_ready"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}
