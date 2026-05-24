package integration

import (
	"net/http"
	"testing"
	"time"
)

// TestLogoutRevokesExistingToken pins the user-visible promise:
// once /api/auth/logout returns, the bearer token used to make that
// call cannot be re-used for any other authenticated endpoint. The
// guarantee is what makes "Sign out" a real security action rather
// than a UI animation — without it, a stolen token remains usable
// until its natural TTL elapses.
//
// We exercise the full path: signup, hit a protected endpoint, log
// out, hit the same protected endpoint again, expect 401. The
// success-then-fail pair rules out a regression where the middleware
// would 401 even pre-logout (false positive) and a regression where
// logout silently no-ops (false negative).
func TestLogoutRevokesExistingToken(t *testing.T) {
	env := setupEnv(t)
	env.ResetTables()

	tok := env.signupAndLogin("Logout Co", "logout@example.com", "User", "hunter2hunter2")

	// Sanity: token works before logout. Pick a cheap protected
	// endpoint (ListWorkspaces) so we don't accidentally exercise
	// permission-system corner cases.
	status, body := env.httpRequest(http.MethodGet, "/api/workspaces", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("pre-logout workspaces: status=%d body=%s", status, string(body))
	}

	// Logout. The endpoint returns 204 even when the cutoff write
	// fails (best-effort), so the meaningful assertion is on the
	// follow-up request — not on this status code.
	status, body = env.httpRequest(http.MethodPost, "/api/auth/logout", tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("logout: status=%d body=%s", status, string(body))
	}

	// Sleep one second past the logout's cutoff timestamp. RevokeUser
	// rounds to second precision (matching JWT iat semantics) and
	// IsRevoked compares `iat <= cutoff`. Without the pause, the
	// token's iat could be one wall-clock second AFTER the cutoff
	// and the test would incorrectly observe 200 — which is the
	// correct behaviour for the boundary, not a regression. Sleeping
	// here forces the iat-was-before-cutoff branch.
	time.Sleep(1100 * time.Millisecond)

	// Same token, post-logout. Must be 401.
	status, body = env.httpRequest(http.MethodGet, "/api/workspaces", tok.Token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("post-logout workspaces: status=%d body=%s (expected 401)", status, string(body))
	}
}

// TestLogoutRevokesAllExistingTokensForUser checks the multi-device
// invariant: a user who logs in from two devices, then logs out from
// one, has both tokens invalidated. This is the explicit guarantee
// of the per-user cutoff model — we revoke "every token issued at or
// before now", not "this specific session id". Without it the user
// who clicked "log me out everywhere" would still have an old token
// floating around.
func TestLogoutRevokesAllExistingTokensForUser(t *testing.T) {
	env := setupEnv(t)
	env.ResetTables()

	// First login (signup yields a token).
	tok1 := env.signupAndLogin("Multi Co", "multi@example.com", "Multi", "hunter2hunter2")

	// Second login (same credentials, fresh JWT). We pause one
	// second so the second token's iat is strictly greater than
	// the first's — without this both tokens could share an iat
	// and the test wouldn't distinguish "all-revoked" from "by-iat".
	time.Sleep(1100 * time.Millisecond)
	status, body := env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "multi@example.com",
		"password": "hunter2hunter2",
	})
	if status != http.StatusOK {
		t.Fatalf("second login: status=%d body=%s", status, string(body))
	}
	var tok2 tokenPayload
	env.decodeJSON(body, &tok2)

	// Both tokens work pre-logout.
	for i, tk := range []string{tok1.Token, tok2.Token} {
		s, b := env.httpRequest(http.MethodGet, "/api/workspaces", tk, nil)
		if s != http.StatusOK {
			t.Fatalf("pre-logout request %d: status=%d body=%s", i, s, string(b))
		}
	}

	// Logout using tok2 (the newer one).
	if s, b := env.httpRequest(http.MethodPost, "/api/auth/logout", tok2.Token, nil); s != http.StatusNoContent {
		t.Fatalf("logout: status=%d body=%s", s, string(b))
	}

	// Sleep past the cutoff second so both iat values are strictly
	// less than the cutoff (see TestLogoutRevokesExistingToken
	// rationale).
	time.Sleep(1100 * time.Millisecond)

	// Both tokens must now be revoked. This is the multi-device
	// invariant.
	for i, tk := range []string{tok1.Token, tok2.Token} {
		s, b := env.httpRequest(http.MethodGet, "/api/workspaces", tk, nil)
		if s != http.StatusUnauthorized {
			t.Fatalf("post-logout request %d: status=%d body=%s (expected 401)", i, s, string(b))
		}
	}
}

// TestRefreshAfterLogoutIsRejected closes the narrow handler-level
// race the auth handler's Refresh-side IsRevoked check defends
// against: the middleware decision was made against the pre-logout
// cutoff snapshot, but by the time Refresh runs the cutoff is
// durable. Without the handler-side check, a Refresh inside this
// window would mint a *new*, longer-lived token from claims that
// belong to a now-revoked session.
//
// We can't reliably trigger the actual middleware-then-Refresh race
// inside a single test process (the test's "logout request" itself
// flushes the cutoff durably before any subsequent request), but we
// can exercise the handler-level check directly: logout, then call
// Refresh with the same token. The middleware-then-handler path
// runs in series, and the handler's IsRevoked check is the line of
// defence that 401s the Refresh.
func TestRefreshAfterLogoutIsRejected(t *testing.T) {
	env := setupEnv(t)
	env.ResetTables()

	tok := env.signupAndLogin("Refresh Co", "refresh@example.com", "Refresh", "hunter2hunter2")
	if s, b := env.httpRequest(http.MethodPost, "/api/auth/logout", tok.Token, nil); s != http.StatusNoContent {
		t.Fatalf("logout: status=%d body=%s", s, string(b))
	}
	time.Sleep(1100 * time.Millisecond)
	status, body := env.httpRequest(http.MethodPost, "/api/auth/refresh", tok.Token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: status=%d body=%s (expected 401)", status, string(body))
	}
}

// TestLoginAfterLogoutIssuesUsableToken pins the inverse property:
// logout doesn't permanently lock the user out. A fresh login after
// logout must produce a token whose iat is strictly greater than
// the cutoff, hence valid.
func TestLoginAfterLogoutIssuesUsableToken(t *testing.T) {
	env := setupEnv(t)
	env.ResetTables()

	tok := env.signupAndLogin("Relogin Co", "relogin@example.com", "Relogin", "hunter2hunter2")
	if s, b := env.httpRequest(http.MethodPost, "/api/auth/logout", tok.Token, nil); s != http.StatusNoContent {
		t.Fatalf("logout: status=%d body=%s", s, string(b))
	}

	// Sleep so the new login's iat lands strictly after the cutoff
	// second. Without the sleep, login at the same wall-clock
	// second as logout would be revoked by the "iat == cutoff is
	// revoked" rule — a known boundary condition documented on
	// IsRevoked.
	time.Sleep(1100 * time.Millisecond)

	status, body := env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "relogin@example.com",
		"password": "hunter2hunter2",
	})
	if status != http.StatusOK {
		t.Fatalf("re-login: status=%d body=%s", status, string(body))
	}
	var newTok tokenPayload
	env.decodeJSON(body, &newTok)

	if s, b := env.httpRequest(http.MethodGet, "/api/workspaces", newTok.Token, nil); s != http.StatusOK {
		t.Fatalf("new token must work: status=%d body=%s", s, string(b))
	}
}
