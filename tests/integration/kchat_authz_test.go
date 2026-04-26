package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestKChatMutationsRequireAdmin pins the per-handler admin gate on
// the mutating /api/kchat endpoints. Read endpoints (ListRooms /
// GetRoom) and the attachment flow stay open to all members; only
// CreateRoom, DeleteRoom, and SyncMembers require admin role —
// matching the api/drive/handler.go convention for sensitive
// mutations. Without this gate, any workspace member could grant
// themselves admin on a KChat-mapped folder via SyncMembers.
func TestKChatMutationsRequireAdmin(t *testing.T) {
	env := setupEnv(t)
	admin := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw-alice")

	// Admin provisions a room so we have a target id for delete /
	// sync-members.
	const roomID = "kchat-room-authz"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", admin.Token, map[string]string{
		"kchat_room_id": roomID,
	})
	if status != http.StatusCreated {
		t.Fatalf("admin create room: status=%d body=%s", status, string(body))
	}
	var created kchatRoomCreated
	env.decodeJSON(body, &created)

	// Invite a non-admin member and obtain a member-scoped token.
	status, body = env.httpRequest(http.MethodPost, "/api/admin/users", admin.Token, map[string]string{
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
	status, body = env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "bob@acme.test",
		"password": "pw-bob",
	})
	if status != http.StatusOK {
		t.Fatalf("member login: status=%d body=%s", status, string(body))
	}
	var member tokenPayload
	env.decodeJSON(body, &member)

	// CreateRoom: member is forbidden.
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/rooms", member.Token, map[string]string{
		"kchat_room_id": "kchat-room-member",
	})
	if status != http.StatusForbidden {
		t.Errorf("member CreateRoom: expected 403, got %d", status)
	}

	// SyncMembers: member is forbidden — without this gate, the
	// member could escalate themselves to admin on the folder.
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/rooms/"+created.ID.String()+"/sync-members", member.Token, map[string]any{
		"members": []map[string]string{
			{"user_id": invited.ID.String(), "role": "admin"},
		},
	})
	if status != http.StatusForbidden {
		t.Errorf("member SyncMembers: expected 403, got %d", status)
	}

	// DeleteRoom: member is forbidden.
	status, _ = env.httpRequest(http.MethodDelete, "/api/kchat/rooms/"+created.ID.String(), member.Token, nil)
	if status != http.StatusForbidden {
		t.Errorf("member DeleteRoom: expected 403, got %d", status)
	}

	// Read endpoints remain open: ListRooms and GetRoom both 200.
	status, _ = env.httpRequest(http.MethodGet, "/api/kchat/rooms", member.Token, nil)
	if status != http.StatusOK {
		t.Errorf("member ListRooms: expected 200, got %d", status)
	}
	status, _ = env.httpRequest(http.MethodGet, "/api/kchat/rooms/"+created.ID.String(), member.Token, nil)
	if status != http.StatusOK {
		t.Errorf("member GetRoom: expected 200, got %d", status)
	}
}
