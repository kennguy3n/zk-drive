package integration

import (
	"net/http"
	"testing"
)

func TestSignupCreatesWorkspaceAndReturnsToken(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	if tok.Token == "" {
		t.Fatal("expected non-empty token")
	}
	if tok.Role != "admin" {
		t.Errorf("expected role=admin, got %q", tok.Role)
	}
	if tok.WorkspaceID == "" || tok.UserID == "" {
		t.Errorf("expected workspace_id and user_id in response, got %+v", tok)
	}
}

func TestLoginWithCorrectCredentials(t *testing.T) {
	env := setupEnv(t)

	env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	status, body := env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "correct-horse",
	})
	if status != http.StatusOK {
		t.Fatalf("login: status=%d body=%s", status, string(body))
	}
	var tok tokenPayload
	env.decodeJSON(body, &tok)
	if tok.Token == "" {
		t.Fatal("expected token on login")
	}
}

func TestLoginWithWrongPassword(t *testing.T) {
	env := setupEnv(t)

	env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	status, _ := env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "wrong",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", status)
	}
}

func TestProtectedEndpointRequiresToken(t *testing.T) {
	env := setupEnv(t)

	status, _ := env.httpRequest(http.MethodGet, "/api/workspaces", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", status)
	}
}

func TestProtectedEndpointWithValidToken(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	status, _ := env.httpRequest(http.MethodGet, "/api/workspaces", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", status)
	}
}

func TestRefreshIssuesNewToken(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	status, body := env.httpRequest(http.MethodPost, "/api/auth/refresh", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", status, string(body))
	}
	var refreshed tokenPayload
	env.decodeJSON(body, &refreshed)
	if refreshed.Token == "" {
		t.Fatal("expected refreshed token")
	}
}
