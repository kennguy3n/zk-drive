package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

// kchatRoomCreated mirrors api/kchat/handler.go's createRoomRequest
// + the server's RoomFolder response so the test stays aligned with
// the HTTP contract.
type kchatRoomCreated struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	KChatRoomID string    `json:"kchat_room_id"`
	FolderID    uuid.UUID `json:"folder_id"`
	CreatedBy   uuid.UUID `json:"created_by"`
}

// TestKChatRoomFolderMapping exercises the full lifecycle: create a
// mapping (which provisions a folder + admin grant on the creator),
// list / get the mapping, attempt a duplicate (409), and delete it.
// The backing folder is intentionally left in place after delete —
// this test pins that behaviour so a future refactor that adds a
// cascade has to update the contract here first.
func TestKChatRoomFolderMapping(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	const roomID = "kchat-room-1234"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": roomID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", status, string(body))
	}
	var created kchatRoomCreated
	env.decodeJSON(body, &created)
	if created.ID == uuid.Nil {
		t.Fatalf("create: room id not set: %+v", created)
	}
	if created.FolderID == uuid.Nil {
		t.Fatalf("create: folder id not set")
	}
	if created.KChatRoomID != roomID {
		t.Errorf("create: kchat_room_id mismatch: %q", created.KChatRoomID)
	}

	// The provisioned folder exists and is fetchable through the
	// regular folder API, with the prefix the service uses.
	status, body = env.httpRequest(http.MethodGet, "/api/folders/"+created.FolderID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get folder: status=%d body=%s", status, string(body))
	}
	var folderResp struct {
		Folder folder.Folder `json:"folder"`
	}
	env.decodeJSON(body, &folderResp)
	if folderResp.Folder.Name != "KChat: "+roomID {
		t.Errorf("folder name: expected %q, got %q", "KChat: "+roomID, folderResp.Folder.Name)
	}

	// Creator received an admin grant on the room folder.
	status, body = env.httpRequest(http.MethodGet, "/api/permissions?resource_type=folder&resource_id="+created.FolderID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list permissions: status=%d body=%s", status, string(body))
	}
	var perms struct {
		Permissions []permission.Permission `json:"permissions"`
	}
	env.decodeJSON(body, &perms)
	var foundAdmin bool
	for _, p := range perms.Permissions {
		if p.GranteeID.String() == tok.UserID && p.Role == permission.RoleAdmin {
			foundAdmin = true
		}
	}
	if !foundAdmin {
		t.Errorf("creator admin grant missing on room folder, got %+v", perms.Permissions)
	}

	// List surfaces the new mapping.
	status, body = env.httpRequest(http.MethodGet, "/api/kchat/rooms", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list rooms: status=%d body=%s", status, string(body))
	}
	var list struct {
		Rooms []kchatRoomCreated `json:"rooms"`
	}
	env.decodeJSON(body, &list)
	if len(list.Rooms) != 1 || list.Rooms[0].ID != created.ID {
		t.Errorf("list: expected created room, got %+v", list.Rooms)
	}

	// Get returns the same shape.
	status, body = env.httpRequest(http.MethodGet, "/api/kchat/rooms/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get room: status=%d body=%s", status, string(body))
	}

	// A duplicate POST is a 409, not a 200.
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": roomID,
	})
	if status != http.StatusConflict {
		t.Errorf("duplicate POST: expected 409, got %d", status)
	}

	// Empty room id is a 400.
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": "   ",
	})
	if status != http.StatusBadRequest {
		t.Errorf("blank room id: expected 400, got %d", status)
	}

	// Delete removes the mapping but leaves the folder.
	status, _ = env.httpRequest(http.MethodDelete, "/api/kchat/rooms/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Errorf("delete: expected 204, got %d", status)
	}
	status, _ = env.httpRequest(http.MethodGet, "/api/kchat/rooms/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("get after delete: expected 404, got %d", status)
	}
	// Folder still exists.
	status, _ = env.httpRequest(http.MethodGet, "/api/folders/"+created.FolderID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Errorf("folder after delete: expected 200, got %d", status)
	}
	// Re-mapping the same room id with a fresh suffix succeeds —
	// the workspace × room uniqueness key was the only blocker.
	// Re-using the exact same room id without first deleting the
	// underlying folder is intentionally not supported; the regular
	// folder API (or a follow-up cascading-delete change) is the
	// correct way to fully reset the state.
	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": roomID + "-v2",
	})
	if status != http.StatusCreated {
		t.Errorf("re-map distinct id: expected 201, got %d body=%s", status, string(body))
	}
}

// kchatModelsCheck pins the package-level constants exposed for
// permission roles. Catching a drift here surfaces the issue without
// reaching for runtime tests.
func TestKChatRoleConstantsMatchPermissionPackage(t *testing.T) {
	if kchat.RoleAdmin != permission.RoleAdmin {
		t.Errorf("admin role drift: kchat=%q permission=%q", kchat.RoleAdmin, permission.RoleAdmin)
	}
	if kchat.RoleEditor != permission.RoleEditor {
		t.Errorf("editor role drift: kchat=%q permission=%q", kchat.RoleEditor, permission.RoleEditor)
	}
	if kchat.RoleViewer != permission.RoleViewer {
		t.Errorf("viewer role drift: kchat=%q permission=%q", kchat.RoleViewer, permission.RoleViewer)
	}
}
