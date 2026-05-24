package integration

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/workspace"
)

func TestCreateWorkspace(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	status, body := env.httpRequest(http.MethodPost, "/api/workspaces", tok.Token, map[string]string{
		"name": "Acme-Secondary",
	})
	if status != http.StatusCreated {
		t.Fatalf("create workspace: status=%d body=%s", status, string(body))
	}
	var ws workspace.Workspace
	env.decodeJSON(body, &ws)
	if ws.Name != "Acme-Secondary" {
		t.Errorf("name mismatch: %q", ws.Name)
	}
}

func TestGetWorkspaceByID(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	status, body := env.httpRequest(http.MethodGet, "/api/workspaces/"+tok.WorkspaceID, tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get workspace: status=%d body=%s", status, string(body))
	}
	var ws workspace.Workspace
	env.decodeJSON(body, &ws)
	if ws.ID.String() != tok.WorkspaceID {
		t.Errorf("id mismatch: %s vs %s", ws.ID, tok.WorkspaceID)
	}
}

func TestUpdateWorkspaceName(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	name := "Acme Renamed"
	status, body := env.httpRequest(http.MethodPut, "/api/workspaces/"+tok.WorkspaceID, tok.Token, map[string]any{
		"name": name,
	})
	if status != http.StatusOK {
		t.Fatalf("update workspace: status=%d body=%s", status, string(body))
	}
	var ws workspace.Workspace
	env.decodeJSON(body, &ws)
	if ws.Name != name {
		t.Errorf("expected name=%q got %q", name, ws.Name)
	}
}

func TestListWorkspacesForUser(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")
	status, body := env.httpRequest(http.MethodGet, "/api/workspaces", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list workspaces: status=%d body=%s", status, string(body))
	}
	var out struct {
		Workspaces []workspace.Workspace `json:"workspaces"`
	}
	env.decodeJSON(body, &out)
	if len(out.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(out.Workspaces))
	}
	if out.Workspaces[0].ID.String() != tok.WorkspaceID {
		t.Errorf("workspace id mismatch")
	}
}

func TestWorkspaceTenantIsolation(t *testing.T) {
	env := setupEnv(t)

	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "password-1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "password-2")

	// Bob may not read Alice's workspace even though he has a valid token.
	status, _ := env.httpRequest(http.MethodGet, "/api/workspaces/"+alice.WorkspaceID, bob.Token, nil)
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-tenant workspace read, got %d", status)
	}
}

// TestCreateWorkspaceForbiddenForMemberRole pins the admin-only invariant
// that members cannot self-promote by spinning up a fresh workspace
// (which would make them admin of the new workspace). Only admins
// of an existing workspace may create new ones.
func TestCreateWorkspaceForbiddenForMemberRole(t *testing.T) {
	env := setupEnv(t)

	admin := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "admin-pw")
	// Admin invites a member into the same workspace via the
	// existing admin invite endpoint.
	status, _ := env.httpRequest(http.MethodPost, "/api/admin/users", admin.Token, map[string]string{
		"email":    "bob@acme.test",
		"name":     "Bob",
		"password": "member-pw",
		"role":     "member",
	})
	if status != http.StatusCreated {
		t.Fatalf("invite member: status=%d", status)
	}
	// Bob logs in to get a member-scoped JWT.
	status, body := env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "bob@acme.test",
		"password": "member-pw",
	})
	if status != http.StatusOK {
		t.Fatalf("member login: status=%d body=%s", status, string(body))
	}
	var member tokenPayload
	env.decodeJSON(body, &member)
	if member.Role != "member" {
		t.Fatalf("expected member role, got %q", member.Role)
	}

	// Bob attempts to create a workspace. Expect 403.
	status, body = env.httpRequest(http.MethodPost, "/api/workspaces", member.Token, map[string]string{
		"name": "Bob-Side-Project",
	})
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 on member workspace creation, got status=%d body=%s", status, string(body))
	}
}

// TestCreateWorkspaceAdminStillAllowed is the positive control for
// TestCreateWorkspaceForbiddenForMemberRole — proves the gate
// doesn't accidentally lock out admins (who legitimately need to
// create additional workspaces).
func TestCreateWorkspaceAdminStillAllowed(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "admin-pw")
	if tok.Role != "admin" {
		t.Fatalf("setup invariant: expected admin role, got %q", tok.Role)
	}
	status, body := env.httpRequest(http.MethodPost, "/api/workspaces", tok.Token, map[string]string{
		"name": "Acme-Secondary",
	})
	if status != http.StatusCreated {
		t.Fatalf("admin create workspace: status=%d body=%s", status, string(body))
	}
}
