package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// wrongLogin attempts a sign-in with a bad password and returns the
// HTTP status plus the decoded error code (empty when the body has no
// code field).
func (e *testEnv) wrongLogin(email string) (int, string) {
	e.t.Helper()
	status, body := e.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    email,
		"password": "definitely-wrong",
	})
	var er struct {
		Code string `json:"code"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &er)
	}
	return status, er.Code
}

// TestLoginBruteForceCooldown drives the IP-reputation guard (6.3)
// through the real HTTP stack: a burst of failed sign-ins from one IP
// crosses the failure threshold and the next attempt is rejected with
// 429 AUTH_TOO_MANY_ATTEMPTS + Retry-After, while a subsequent correct
// sign-in clears the IP's reputation.
//
// Harness config (setup_test.go): threshold=3, delays=[200ms,500ms],
// block=1s.
func TestLoginBruteForceCooldown(t *testing.T) {
	env := setupEnv(t)
	env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	// Attempts 1 and 2 are below the threshold → plain 401s, no cooldown.
	for i := 0; i < 2; i++ {
		if status, code := env.wrongLogin("admin@acme.test"); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d (code=%s)", i+1, status, code)
		}
	}

	// Attempt 3 still returns 401 (credentials are wrong) but arms the
	// first cooldown.
	if status, _ := env.wrongLogin("admin@acme.test"); status != http.StatusUnauthorized {
		t.Fatalf("threshold attempt: expected 401, got %d", status)
	}

	// Attempt 4, issued immediately, lands inside the cooldown window
	// and is throttled before the credential check even runs.
	status, body := env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "definitely-wrong",
	})
	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429 during cooldown, got %d body=%s", status, string(body))
	}
	var er struct {
		Code string `json:"code"`
	}
	env.decodeJSON(body, &er)
	if er.Code != "AUTH_TOO_MANY_ATTEMPTS" {
		t.Fatalf("expected AUTH_TOO_MANY_ATTEMPTS, got %q", er.Code)
	}

	// A correct sign-in after the cooldown elapses must succeed and
	// reset the IP's reputation.
	time.Sleep(300 * time.Millisecond)
	tok := env.login("admin@acme.test", "correct-horse")
	if tok.Token == "" {
		t.Fatal("expected successful login after cooldown to return a token")
	}

	// Reputation was reset: two more wrong attempts are both below the
	// (fresh) threshold and return 401 rather than 429.
	for i := 0; i < 2; i++ {
		if status, code := env.wrongLogin("admin@acme.test"); status != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: expected 401, got %d (code=%s)", i+1, status, code)
		}
	}
}
