package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// TestPhase4DecisionGate pins the Phase 4 decision-gate scenario in
// a single end-to-end test. The contract this closes out is:
//
//   - A strict-ZK folder can be created, uploaded into (metadata
//     only), and its file name never surfaces through the FTS search
//     endpoint — server-side processing is off by design.
//   - A KChat room maps to a Drive folder; an attachment can be
//     round-tripped through the room's upload-url + confirm endpoints.
//   - An admin can seed a client room from the "agency" template and
//     receive the four standard sub-folders.
//
// The test uses only the HTTP harness in setup_test.go so it mirrors
// what a real client sees on the wire.
func TestPhase4DecisionGate(t *testing.T) {
	env := setupEnv(t)
	admin := env.signupAndLogin("Acme Privacy", "admin@acme.test", "Alice", "pw")

	// ---------- 1. Strict-ZK folder: no preview, no search ----------
	status, body := env.httpRequest(http.MethodPost, "/api/folders", admin.Token, map[string]string{
		"name":            "Vault",
		"encryption_mode": folder.EncryptionStrictZK,
	})
	if status != http.StatusCreated {
		t.Fatalf("strict-zk folder: status=%d body=%s", status, string(body))
	}
	var vault folder.Folder
	env.decodeJSON(body, &vault)
	if vault.EncryptionMode != folder.EncryptionStrictZK {
		t.Fatalf("expected strict_zk, got %q", vault.EncryptionMode)
	}

	const distinctiveName = "phase4gatesecret"
	// createFile performs the file-metadata row write the gate cares
	// about. We don't PUT bytes because the test harness stubs S3 —
	// the strict-ZK contract is tested at the metadata + search layer.
	zkFile := createFile(t, env, admin.Token, vault.ID.String(), distinctiveName+" report", "text/plain")

	status, body = env.httpRequest(http.MethodGet, "/api/search?q="+distinctiveName, admin.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search strict-zk: status=%d body=%s", status, string(body))
	}
	var searchResp struct {
		Hits []struct {
			ID   uuid.UUID `json:"id"`
			Type string    `json:"type"`
		} `json:"hits"`
	}
	env.decodeJSON(body, &searchResp)
	for _, h := range searchResp.Hits {
		if h.ID == zkFile.ID {
			t.Fatalf("phase4 gate: strict-ZK file leaked into search: %+v", searchResp.Hits)
		}
	}

	// ---------- 2. KChat attachment round-trip ----------
	const kchatRoomID = "kchat-phase4-gate"
	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms", admin.Token, map[string]string{
		"kchat_room_id": kchatRoomID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create kchat room: status=%d body=%s", status, string(body))
	}
	var room kchatRoomCreated
	env.decodeJSON(body, &room)

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/attachments/upload-url", admin.Token,
		map[string]any{
			"kchat_room_id": kchatRoomID,
			"filename":      "decision.pdf",
			"mime_type":     "application/pdf",
			"size_bytes":    2048,
		})
	if status != http.StatusOK {
		t.Fatalf("attachment upload-url: status=%d body=%s", status, string(body))
	}
	var upload struct {
		UploadURL string    `json:"upload_url"`
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
		FolderID  uuid.UUID `json:"folder_id"`
	}
	env.decodeJSON(body, &upload)
	if upload.UploadURL == "" || upload.UploadID == uuid.Nil {
		t.Fatalf("attachment upload response incomplete: %+v", upload)
	}
	if upload.FolderID != room.FolderID {
		t.Fatalf("attachment folder mismatch: %s vs %s", upload.FolderID, room.FolderID)
	}

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", admin.Token,
		map[string]any{
			"file_id":    upload.UploadID.String(),
			"object_key": upload.ObjectKey,
			"checksum":   "sha256:phase4gate",
			"size_bytes": 2048,
		})
	if status != http.StatusOK {
		t.Fatalf("attachment confirm: status=%d body=%s", status, string(body))
	}

	// Verify the attachment is reachable through the regular file
	// metadata API — proof that KChat attachments flow into the
	// same drive catalogue as everything else.
	status, body = env.httpRequest(http.MethodGet, "/api/files/"+upload.UploadID.String(), admin.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get attachment metadata: status=%d body=%s", status, string(body))
	}

	// ---------- 3. Client-room template picker ----------
	status, body = env.httpRequest(http.MethodGet, "/api/client-rooms/templates", admin.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list templates: status=%d body=%s", status, string(body))
	}
	var templatesResp struct {
		Templates []struct {
			Name       string   `json:"name"`
			SubFolders []string `json:"sub_folders"`
		} `json:"templates"`
	}
	env.decodeJSON(body, &templatesResp)
	if len(templatesResp.Templates) == 0 {
		t.Fatalf("expected non-empty template list")
	}
	foundAgency := false
	for _, tpl := range templatesResp.Templates {
		if tpl.Name == "agency" {
			foundAgency = true
			if len(tpl.SubFolders) == 0 {
				t.Errorf("agency template has no sub-folders listed")
			}
		}
	}
	if !foundAgency {
		t.Fatalf("agency template missing from response: %+v", templatesResp.Templates)
	}

	status, body = env.httpRequest(http.MethodPost, "/api/client-rooms/from-template", admin.Token,
		map[string]any{
			"name":     "Globex — Phase 4 Review",
			"template": "agency",
		})
	if status != http.StatusCreated {
		t.Fatalf("from-template: status=%d body=%s", status, string(body))
	}
	var created struct {
		ID           uuid.UUID   `json:"id"`
		FolderID     uuid.UUID   `json:"folder_id"`
		SubFolderIDs []uuid.UUID `json:"sub_folder_ids"`
	}
	env.decodeJSON(body, &created)
	if created.FolderID == uuid.Nil {
		t.Fatalf("client-room missing folder_id: %+v", created)
	}
	if len(created.SubFolderIDs) != 4 {
		t.Fatalf("agency template: expected 4 sub-folders, got %d", len(created.SubFolderIDs))
	}

	// The sub-folders should show up under the room folder.
	status, body = env.httpRequest(http.MethodGet, "/api/folders/"+created.FolderID.String(), admin.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get room folder: status=%d body=%s", status, string(body))
	}
	var folderResp struct {
		Children []folder.Folder `json:"children"`
	}
	env.decodeJSON(body, &folderResp)
	if len(folderResp.Children) != 4 {
		names := make([]string, 0, len(folderResp.Children))
		for _, c := range folderResp.Children {
			names = append(names, c.Name)
		}
		t.Fatalf("expected 4 agency sub-folders, got %d: [%s]",
			len(folderResp.Children), strings.Join(names, ", "))
	}
}
