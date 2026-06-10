package integration

import (
	"net/http"
	"testing"
)

// sessionInfoView mirrors the api/auth list response shape (the
// handler's struct is unexported). Only the fields the tests assert
// on are decoded.
type sessionInfoView struct {
	SessionID  string `json:"session_id"`
	UserAgent  string `json:"user_agent"`
	IP         string `json:"ip"`
	Current    bool   `json:"current"`
	LastSeenAt string `json:"last_seen_at"`
}

type sessionsListView struct {
	Sessions []sessionInfoView `json:"sessions"`
}

func (e *testEnv) listSessions(token string) sessionsListView {
	e.t.Helper()
	status, body := e.httpRequest(http.MethodGet, "/api/auth/sessions", token, nil)
	if status != http.StatusOK {
		e.t.Fatalf("list sessions: status=%d body=%s", status, string(body))
	}
	var out sessionsListView
	e.decodeJSON(body, &out)
	return out
}

// TestListSessionsReturnsCurrentDevice verifies that after sign-in the
// caller sees exactly one active session, flagged current, carrying the
// device info (User-Agent + IP) captured at login.
func TestListSessionsReturnsCurrentDevice(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	list := env.listSessions(tok.Token)
	if len(list.Sessions) != 1 {
		t.Fatalf("expected exactly 1 session, got %d (%+v)", len(list.Sessions), list.Sessions)
	}
	s := list.Sessions[0]
	if !s.Current {
		t.Errorf("the only session must be flagged current")
	}
	if s.SessionID == "" {
		t.Errorf("session id must be populated")
	}
	if s.IP == "" || s.UserAgent == "" {
		t.Errorf("device info must be captured: ua=%q ip=%q", s.UserAgent, s.IP)
	}
}

// TestRevokeSpecificSessionRemovesIt signs in twice for the same user
// (two device sessions), then revokes the older one and confirms it
// disappears from the list while the current one survives.
func TestRevokeSpecificSessionRemovesIt(t *testing.T) {
	env := setupEnv(t)
	env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	first := env.login("admin@acme.test", "correct-horse")
	second := env.login("admin@acme.test", "correct-horse")

	// The newest token's view should list multiple sessions; pick one
	// that is NOT the current session to revoke.
	list := env.listSessions(second.Token)
	if len(list.Sessions) < 2 {
		t.Fatalf("expected >= 2 sessions, got %d", len(list.Sessions))
	}
	var victim string
	for _, s := range list.Sessions {
		if !s.Current {
			victim = s.SessionID
			break
		}
	}
	if victim == "" {
		t.Fatal("could not find a non-current session to revoke")
	}

	status, body := env.httpRequest(http.MethodDelete, "/api/auth/sessions/"+victim, second.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("revoke: status=%d body=%s", status, string(body))
	}

	after := env.listSessions(second.Token)
	for _, s := range after.Sessions {
		if s.SessionID == victim {
			t.Fatalf("revoked session %s still present", victim)
		}
	}
	if len(after.Sessions) != len(list.Sessions)-1 {
		t.Fatalf("expected one fewer session after revoke: before=%d after=%d", len(list.Sessions), len(after.Sessions))
	}
	_ = first
}

// TestRevokeSessionCrossTenantIsScoped pins that a caller cannot revoke
// a session that belongs to a different user/workspace: the store keys
// revocation by the JWT's workspace+user, so another tenant's session
// id resolves to "not found" (404) rather than being deleted.
func TestRevokeSessionCrossTenantIsScoped(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	bob := env.signupAndLogin("Globex", "admin@globex.test", "Bob", "correct-horse")

	aliceSessions := env.listSessions(alice.Token)
	if len(aliceSessions.Sessions) == 0 {
		t.Fatal("alice should have a session")
	}
	target := aliceSessions.Sessions[0].SessionID

	// Bob attempts to revoke Alice's session id.
	status, _ := env.httpRequest(http.MethodDelete, "/api/auth/sessions/"+target, bob.Token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke must 404, got %d", status)
	}

	// Alice's session is untouched.
	still := env.listSessions(alice.Token)
	found := false
	for _, s := range still.Sessions {
		if s.SessionID == target {
			found = true
		}
	}
	if !found {
		t.Fatal("alice's session must survive bob's revoke attempt")
	}
}

// TestSessionAnomalyForcesReauth verifies the device-anomaly gate end
// to end: a token minted under one User-Agent is rejected (401,
// AUTH_SESSION_ANOMALY) when replayed from a clearly different
// User-Agent, while the original device keeps working.
func TestSessionAnomalyForcesReauth(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	// Same device → still authorized.
	if status, body := env.httpRequest(http.MethodGet, "/api/auth/sessions", tok.Token, nil); status != http.StatusOK {
		t.Fatalf("same-device request must pass: status=%d body=%s", status, string(body))
	}

	// Different User-Agent → anomaly → 401 with the dedicated code.
	status, body := env.httpRequestWithHeaders(http.MethodGet, "/api/auth/sessions", tok.Token, nil, map[string]string{
		"User-Agent": "Mozilla/5.0 (totally-different-device) Safari/9999",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("anomalous device must be rejected: status=%d body=%s", status, string(body))
	}
	var er struct {
		Code string `json:"code"`
	}
	env.decodeJSON(body, &er)
	if er.Code != "AUTH_SESSION_ANOMALY" {
		t.Fatalf("expected AUTH_SESSION_ANOMALY, got %q (body=%s)", er.Code, string(body))
	}
}
