package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

// listAuditEntries fetches the audit log for the caller's workspace.
// Audit writes are async (fire-and-forget background worker), so callers
// poll via waitForAuditAction below rather than calling this directly.
func listAuditEntries(t *testing.T, env *testEnv, token, action string) []audit.Entry {
	t.Helper()
	path := "/api/admin/audit-log?limit=50"
	if action != "" {
		path += "&action=" + action
	}
	status, body := env.httpRequest(http.MethodGet, path, token, nil)
	if status != http.StatusOK {
		t.Fatalf("audit-log: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Entries []audit.Entry `json:"entries"`
	}
	env.decodeJSON(body, &resp)
	return resp.Entries
}

// waitForAuditAction polls /api/admin/audit-log until at least one row
// with the given action appears, or the deadline elapses.
func waitForAuditAction(t *testing.T, env *testEnv, token, action string) audit.Entry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries := listAuditEntries(t, env, token, action)
		if len(entries) > 0 {
			return entries[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("audit entry with action=%q never appeared", action)
	return audit.Entry{}
}

func TestAdminListUsers(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodGet, "/api/admin/users", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list users: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Users []struct {
			ID    uuid.UUID `json:"id"`
			Email string    `json:"email"`
			Role  string    `json:"role"`
		} `json:"users"`
	}
	env.decodeJSON(body, &resp)
	if len(resp.Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(resp.Users))
	}
	got := resp.Users[0]
	if got.Email != "admin@acme.test" {
		t.Errorf("expected admin email, got %q", got.Email)
	}
	if got.Role != "admin" {
		t.Errorf("expected role=admin, got %q", got.Role)
	}
}

func TestAdminDeactivateUser(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodPost, "/api/admin/users", tok.Token, map[string]string{
		"email":    "bob@acme.test",
		"name":     "Bob",
		"password": "pw-bob",
		"role":     "member",
	})
	if status != http.StatusCreated {
		t.Fatalf("invite user: status=%d body=%s", status, string(body))
	}
	var invited struct {
		ID uuid.UUID `json:"id"`
	}
	env.decodeJSON(body, &invited)
	if invited.ID == uuid.Nil {
		t.Fatal("invited user id is nil")
	}

	status, _ = env.httpRequest(http.MethodDelete, "/api/admin/users/"+invited.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("deactivate: expected 204, got %d", status)
	}

	// Listing now returns the row with deactivated_at set so audit
	// history still resolves the actor; "no longer active" is encoded
	// as deactivated_at != nil in the response.
	status, body = env.httpRequest(http.MethodGet, "/api/admin/users", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list users post-deactivate: %d", status)
	}
	var resp struct {
		Users []struct {
			ID            uuid.UUID  `json:"id"`
			DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
		} `json:"users"`
	}
	env.decodeJSON(body, &resp)
	for _, u := range resp.Users {
		if u.ID == invited.ID && u.DeactivatedAt == nil {
			t.Fatal("expected invited user to have deactivated_at set after DELETE")
		}
	}
}

func TestAuditLogRecordsLogin(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Trigger an explicit login so we deterministically write an
	// auth.login entry (signup only writes auth.signup).
	status, _ := env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "pw",
	})
	if status != http.StatusOK {
		t.Fatalf("login: status=%d", status)
	}

	entry := waitForAuditAction(t, env, tok.Token, audit.ActionLogin)
	if !strings.Contains(entry.Action, "login") {
		t.Errorf("expected action containing \"login\", got %q", entry.Action)
	}
}

func TestAuditLogRecordsPermissionGrant(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	f := createFolder(t, env, tok.Token, nil, "Engineering")
	status, _ := grantPermission(t, env, tok.Token, map[string]any{
		"resource_type": permission.ResourceFolder,
		"resource_id":   f.ID.String(),
		"grantee_type":  permission.GranteeGuest,
		"grantee_id":    "00000000-0000-0000-0000-000000000099",
		"role":          permission.RoleViewer,
	})
	if status != http.StatusCreated {
		t.Fatalf("grant: status=%d", status)
	}

	entry := waitForAuditAction(t, env, tok.Token, audit.ActionPermissionGrant)
	if entry.Action != audit.ActionPermissionGrant {
		t.Errorf("expected action=%q, got %q", audit.ActionPermissionGrant, entry.Action)
	}
}
