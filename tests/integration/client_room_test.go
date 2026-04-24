package integration

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/sharing"
)

// createClientRoomPayload mirrors api/drive/handler.go's
// createClientRoomRequest so test bodies stay aligned with the HTTP
// contract.
type createClientRoomPayload struct {
	Name           string `json:"name"`
	Password       string `json:"password,omitempty"`
	DropboxEnabled bool   `json:"dropbox_enabled,omitempty"`
}

// TestCreateAndListClientRoom walks the full lifecycle: create a
// room, verify the response envelopes a folder + share link token,
// list rooms, and delete the room. Deletion is not yet contract-
// verified to cascade to share-link revocation; this test pins only
// the HTTP status codes and the payload shape so future refactors
// keep the API stable.
func TestCreateAndListClientRoom(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodPost, "/api/client-rooms", tok.Token, createClientRoomPayload{
		Name:           "Q1 Deliverables",
		DropboxEnabled: false,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var created struct {
		sharing.ClientRoom
		ShareLinkToken string `json:"share_link_token"`
	}
	env.decodeJSON(body, &created)
	if created.ID.String() == "" {
		t.Fatalf("create: room id not set: %+v", created)
	}
	if created.FolderID.String() == "" {
		t.Fatalf("create: folder id not set")
	}
	if created.ShareLinkToken == "" {
		t.Fatalf("create: share_link_token missing")
	}

	status, body = env.httpRequest(http.MethodGet, "/api/client-rooms", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", status, string(body))
	}
	var list struct {
		Rooms []sharing.ClientRoom `json:"rooms"`
	}
	env.decodeJSON(body, &list)
	var found bool
	for _, r := range list.Rooms {
		if r.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("list: expected created room to appear, got %+v", list.Rooms)
	}

	status, body = env.httpRequest(http.MethodGet, "/api/client-rooms/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", status, string(body))
	}

	status, body = env.httpRequest(http.MethodDelete, "/api/client-rooms/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Errorf("delete: expected 204, got %d body=%s", status, string(body))
	}
}

// TestCreateClientRoomRejectsBlankName pins the 400 contract for
// room creation with an empty name.
func TestCreateClientRoomRejectsBlankName(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, _ := env.httpRequest(http.MethodPost, "/api/client-rooms", tok.Token, createClientRoomPayload{
		Name: "",
	})
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for blank name, got %d", status)
	}
}
