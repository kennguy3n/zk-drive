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
// k8s probe timeout (default 1s). Failures are reported in the JSON
// response by checker name + a generic "fail" status; the verbose
// underlying error is written to the server logs only, so /readyz
// stays safe to expose even if a misconfigured ingress accidentally
// reaches it. See ReadyHandler for the rationale.
package health

import (
	"context"
	"encoding/json"

	"net/http"
	"sync"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// DefaultCheckTimeout is the per-check timeout applied by Service
// when no explicit timeout is configured. 900ms is deliberately just
// under the default k8s readiness probe timeoutSeconds of 1s, so
// every individual dependency check has a chance to complete (or be
// cancelled and reported as failed) before k8s gives up on the whole
// HTTP response and marks the probe as timed-out.
//
// Operators who tune the k8s probe timeoutSeconds upward (e.g. to 3s
// for environments where a cross-AZ S3 HeadBucket can legitimately
// take >1s) should pass a matching larger timeout to NewService
// rather than relying on the default — otherwise the check budget
// stays at 900ms and a slow-but-healthy dependency will still flap
// the pod out of the service mesh.
const DefaultCheckTimeout = 900 * time.Millisecond

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
// checker's Name() to "ok" or "fail" — by design we never leak the
// underlying error string into the response (see ReadyHandler).
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
//
// Information-disclosure posture: the response body identifies WHICH
// checker failed (by Name()) but never echoes the underlying error
// string back to the caller. Verbatim error messages from net/Go SDK
// stacks routinely embed dial tcp addresses, container hostnames, or
// internal DNS names, which would leak network topology to anyone
// who could reach /readyz (e.g. if a misconfigured ingress exposes
// the path to the internet). Operators get the full error context
// from the server logs instead, where one slog record per failed
// check is emitted with the checker name + error so it correlates
// to the response body's "fail" entry.
func (s *Service) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		results := make(map[string]string, len(s.checkers))
		type checkFailure struct {
			name string
			err  error
		}
		var (
			mu       sync.Mutex
			wg       sync.WaitGroup
			allOK    = true
			failures []checkFailure
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
					// Response carries the name + generic "fail"
					// only; the verbose error goes to the logs.
					results[c.Name()] = "fail"
					failures = append(failures, checkFailure{name: c.Name(), err: err})
					allOK = false
				} else {
					results[c.Name()] = "ok"
				}
			}(c)
		}
		wg.Wait()

		// Emit one server-side log line per failed check so the
		// operator-visible response ("fail") can be correlated to a
		// full error in the logs.
		for _, f := range failures {
			logging.FromContext(r.Context()).Error("readyz check failed", "name", f.name, "err", f.err)
		}

		status := http.StatusOK
		body := readyResponse{Status: "ready", Checks: results}
		if !allOK {
			status = http.StatusServiceUnavailable
			body.Status = "not_ready"
		}
		w.Header().Set("Content-Type", "application/json")
		// Defence-in-depth against intermediary caches. The standard
		// k8s kubelet hits the pod directly and never caches, but
		// some mesh / sidecar setups (e.g. Envoy-as-PEP, Cloudflare
		// for non-k8s deployments) can sit in the path. A stale
		// cached 200 would mask a real outage; pinning no-cache /
		// no-store ensures every probe sees fresh dependency state.
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}
