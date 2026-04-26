package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/permission"
)

// TestKChatPermissionSync exercises POST /api/kchat/rooms/{id}/sync-members
// in three rounds: initial sync adds two grants; a second sync
// upgrades one user's role and removes the other; a third sync with
// an empty list revokes all KChat-managed grants. The creator's
// existing admin grant must survive every sync because creator is
// explicitly listed in the desired set on each round.
func TestKChatPermissionSync(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	const roomID = "kchat-sync-room"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": roomID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", status, string(body))
	}
	var created kchatRoomCreated
	env.decodeJSON(body, &created)

	bob := uuid.New()
	carol := uuid.New()

	// Round 1: add bob (viewer) and carol (editor).
	status, body = env.httpRequest(http.MethodPost,
		"/api/kchat/rooms/"+created.ID.String()+"/sync-members", tok.Token,
		map[string]any{
			"members": []map[string]any{
				{"user_id": bob.String(), "role": permission.RoleViewer},
				{"user_id": carol.String(), "role": permission.RoleEditor},
			},
		})
	if status != http.StatusOK {
		t.Fatalf("sync round 1: status=%d body=%s", status, string(body))
	}

	bobRole, carolRole := lookupGrants(t, env, tok.Token, created.FolderID, bob, carol)
	if bobRole != permission.RoleViewer {
		t.Errorf("round 1 bob: expected viewer, got %q", bobRole)
	}
	if carolRole != permission.RoleEditor {
		t.Errorf("round 1 carol: expected editor, got %q", carolRole)
	}

	// Round 2: drop bob, upgrade carol to admin. Idempotency check —
	// re-sending the same set must leave grants untouched (no
	// duplicate rows).
	status, body = env.httpRequest(http.MethodPost,
		"/api/kchat/rooms/"+created.ID.String()+"/sync-members", tok.Token,
		map[string]any{
			"members": []map[string]any{
				{"user_id": carol.String(), "role": permission.RoleAdmin},
			},
		})
	if status != http.StatusOK {
		t.Fatalf("sync round 2: status=%d body=%s", status, string(body))
	}

	bobRole, carolRole = lookupGrants(t, env, tok.Token, created.FolderID, bob, carol)
	if bobRole != "" {
		t.Errorf("round 2 bob: expected revoked, got %q", bobRole)
	}
	if carolRole != permission.RoleAdmin {
		t.Errorf("round 2 carol: expected admin, got %q", carolRole)
	}

	// Re-sync the same set: idempotent.
	status, _ = env.httpRequest(http.MethodPost,
		"/api/kchat/rooms/"+created.ID.String()+"/sync-members", tok.Token,
		map[string]any{
			"members": []map[string]any{
				{"user_id": carol.String(), "role": permission.RoleAdmin},
			},
		})
	if status != http.StatusOK {
		t.Errorf("idempotent sync: status=%d", status)
	}
	carolGrants := countGrants(t, env, tok.Token, created.FolderID, carol)
	if carolGrants != 1 {
		t.Errorf("idempotent sync should leave a single grant, got %d", carolGrants)
	}

	// Round 3: empty member list → all KChat-managed grants revoked.
	status, body = env.httpRequest(http.MethodPost,
		"/api/kchat/rooms/"+created.ID.String()+"/sync-members", tok.Token,
		map[string]any{"members": []map[string]any{}})
	if status != http.StatusOK {
		t.Fatalf("sync round 3: status=%d body=%s", status, string(body))
	}
	if c := countGrants(t, env, tok.Token, created.FolderID, carol); c != 0 {
		t.Errorf("round 3 carol: expected 0 grants, got %d", c)
	}

	// Invalid role is rejected.
	status, _ = env.httpRequest(http.MethodPost,
		"/api/kchat/rooms/"+created.ID.String()+"/sync-members", tok.Token,
		map[string]any{
			"members": []map[string]any{
				{"user_id": uuid.NewString(), "role": "owner"},
			},
		})
	if status != http.StatusBadRequest {
		t.Errorf("invalid role: expected 400, got %d", status)
	}
}

// lookupGrants returns (a, b) the current role for users a and b.
// An empty string means no grant exists for that user.
func lookupGrants(t *testing.T, env *testEnv, token string, folderID, a, b uuid.UUID) (string, string) {
	t.Helper()
	status, body := env.httpRequest(http.MethodGet,
		"/api/permissions?resource_type=folder&resource_id="+folderID.String(), token, nil)
	if status != http.StatusOK {
		t.Fatalf("list permissions: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Permissions []permission.Permission `json:"permissions"`
	}
	env.decodeJSON(body, &resp)
	var roleA, roleB string
	for _, p := range resp.Permissions {
		switch p.GranteeID {
		case a:
			roleA = p.Role
		case b:
			roleB = p.Role
		}
	}
	return roleA, roleB
}

// countGrants returns how many grants exist for userID on folderID.
// SyncMembers is supposed to keep this at 0 or 1 for any given user.
func countGrants(t *testing.T, env *testEnv, token string, folderID, userID uuid.UUID) int {
	t.Helper()
	status, body := env.httpRequest(http.MethodGet,
		"/api/permissions?resource_type=folder&resource_id="+folderID.String(), token, nil)
	if status != http.StatusOK {
		t.Fatalf("list permissions: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Permissions []permission.Permission `json:"permissions"`
	}
	env.decodeJSON(body, &resp)
	n := 0
	for _, p := range resp.Permissions {
		if p.GranteeID == userID {
			n++
		}
	}
	return n
}
