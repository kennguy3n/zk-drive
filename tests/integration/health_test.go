package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestReadyzPassesWithLiveDependencies verifies /readyz returns 200
// when Postgres (the only required dependency in the integration
// harness) is reachable. Optional dependencies (Redis, NATS, S3)
// are intentionally not wired in setupEnv and the health checkers
// short-circuit nil dependencies as OK — exercising the "partial
// configuration" path used by single-process dev stacks.
func TestReadyzPassesWithLiveDependencies(t *testing.T) {
	env := setupEnv(t)

	status, body := env.httpRequest(http.MethodGet, "/readyz", "", nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", status, string(body))
	}
	var resp struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode body: %v body=%s", err, string(body))
	}
	if resp.Status != "ready" {
		t.Fatalf("expected status=ready, got %q (body=%s)", resp.Status, string(body))
	}
	if got := resp.Checks["postgres"]; got != "ok" {
		t.Fatalf("postgres check: expected ok, got %q (body=%s)", got, string(body))
	}
}

// TestReadyzIsPublic verifies /readyz does NOT require authentication.
// k8s probes don't carry bearer tokens, so the route must sit outside
// the AuthMiddleware group.
func TestReadyzIsPublic(t *testing.T) {
	env := setupEnv(t)

	// No token — anonymous request.
	status, _ := env.httpRequest(http.MethodGet, "/readyz", "", nil)
	if status == http.StatusUnauthorized {
		t.Fatalf("expected /readyz to be public, got 401 (route must be outside AuthMiddleware)")
	}
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		t.Fatalf("expected /readyz to return 200 or 503, got %d", status)
	}
}
