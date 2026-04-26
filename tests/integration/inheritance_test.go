package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/permission"
)

func TestChildFileInheritsParentGrant(t *testing.T) {
	env := setupEnv(t)
	admin := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw-alice")

	// Invite a non-admin member; only members are subject to the
	// permission check (admin bypasses assertResourceAccess).
	status, body := env.httpRequest(http.MethodPost, "/api/admin/users", admin.Token, map[string]string{
		"email":    "bob@acme.test",
		"name":     "Bob",
		"password": "pw-bob",
		"role":     "member",
	})
	if status != http.StatusCreated {
		t.Fatalf("invite member: status=%d body=%s", status, string(body))
	}
	var invited struct {
		ID uuid.UUID `json:"id"`
	}
	env.decodeJSON(body, &invited)

	// Login as the new member to obtain a member-scoped token.
	status, body = env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "bob@acme.test",
		"password": "pw-bob",
	})
	if status != http.StatusOK {
		t.Fatalf("member login: status=%d body=%s", status, string(body))
	}
	var memberTok tokenPayload
	env.decodeJSON(body, &memberTok)

	// Admin builds the folder/file hierarchy. The member has no
	// direct grant on the file yet.
	parent := createFolder(t, env, admin.Token, nil, "Engineering")
	child := createFile(t, env, admin.Token, parent.ID.String(), "spec.txt", "text/plain")

	// Without an inherited grant, the member must be denied.
	status, _ = env.httpRequest(http.MethodGet, "/api/files/"+child.ID.String(), memberTok.Token, nil)
	if status == http.StatusOK {
		t.Fatal("expected member to be denied before grant")
	}

	// Grant viewer on the parent folder; the file is reachable
	// through inheritance.
	status, _ = grantPermission(t, env, admin.Token, map[string]any{
		"resource_type": permission.ResourceFolder,
		"resource_id":   parent.ID.String(),
		"grantee_type":  permission.GranteeUser,
		"grantee_id":    invited.ID.String(),
		"role":          permission.RoleViewer,
	})
	if status != http.StatusCreated {
		t.Fatalf("grant inherit: status=%d", status)
	}

	status, _ = env.httpRequest(http.MethodGet, "/api/files/"+child.ID.String(), memberTok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("member should reach child via inherited folder grant, got %d", status)
	}
}
