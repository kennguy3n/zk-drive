package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestKChatAttachmentUpload exercises the room-scoped upload flow:
// request an upload URL (the service creates the file metadata row
// + mints a presigned PUT in the room's folder), then confirm the
// upload (advances the file to a usable version). The test pins the
// HTTP shape and the on-disk effects (file appears in the room
// folder, version row references the right object_key) but does not
// hit a real S3 endpoint — buildTestStorageClient signs against a
// stub URL that's intentionally unreachable.
func TestKChatAttachmentUpload(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	const roomID = "kchat-att-room"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": roomID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", status, string(body))
	}
	var room kchatRoomCreated
	env.decodeJSON(body, &room)

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/attachments/upload-url", tok.Token,
		map[string]any{
			"kchat_room_id": roomID,
			"filename":      "minutes.pdf",
			"mime_type":     "application/pdf",
			"size_bytes":    1024,
		})
	if status != http.StatusOK {
		t.Fatalf("upload-url: status=%d body=%s", status, string(body))
	}
	var uploadResp struct {
		UploadURL string    `json:"upload_url"`
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
		FolderID  uuid.UUID `json:"folder_id"`
	}
	env.decodeJSON(body, &uploadResp)
	if uploadResp.UploadURL == "" {
		t.Errorf("upload_url is empty: %+v", uploadResp)
	}
	if uploadResp.FolderID != room.FolderID {
		t.Errorf("folder_id mismatch: got %s, want %s", uploadResp.FolderID, room.FolderID)
	}
	expectedKeyPrefix := tok.WorkspaceID + "/" + uploadResp.UploadID.String() + "/"
	if !strings.HasPrefix(uploadResp.ObjectKey, expectedKeyPrefix) {
		t.Errorf("object_key %q does not start with %q", uploadResp.ObjectKey, expectedKeyPrefix)
	}

	// Confirm the upload — the service flips the file's
	// current_version pointer to a new version with our object_key.
	status, body = env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
		map[string]any{
			"file_id":    uploadResp.UploadID.String(),
			"object_key": uploadResp.ObjectKey,
			"checksum":   "sha256:abcd",
			"size_bytes": 1024,
		})
	if status != http.StatusOK {
		t.Fatalf("confirm: status=%d body=%s", status, string(body))
	}
	var confirmResp struct {
		FileID    uuid.UUID `json:"file_id"`
		VersionID uuid.UUID `json:"version_id"`
	}
	env.decodeJSON(body, &confirmResp)
	if confirmResp.FileID != uploadResp.UploadID {
		t.Errorf("file_id drift: %s vs %s", confirmResp.FileID, uploadResp.UploadID)
	}
	if confirmResp.VersionID == uuid.Nil {
		t.Error("version_id is nil after confirm")
	}

	// File now lives in the room's folder and its current_version
	// is set, so the regular download flow would resolve it.
	status, body = env.httpRequest(http.MethodGet, "/api/files/"+confirmResp.FileID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get file: status=%d body=%s", status, string(body))
	}
	var f struct {
		ID               uuid.UUID  `json:"id"`
		FolderID         uuid.UUID  `json:"folder_id"`
		CurrentVersionID *uuid.UUID `json:"current_version_id"`
		Name             string     `json:"name"`
	}
	env.decodeJSON(body, &f)
	if f.FolderID != room.FolderID {
		t.Errorf("file landed in wrong folder: got %s, want %s", f.FolderID, room.FolderID)
	}
	if f.CurrentVersionID == nil || *f.CurrentVersionID != confirmResp.VersionID {
		t.Errorf("current_version_id mismatch: got %v, want %s", f.CurrentVersionID, confirmResp.VersionID)
	}
	if f.Name != "minutes.pdf" {
		t.Errorf("filename mismatch: %q", f.Name)
	}

	// A confirm that fakes a foreign object_key is rejected so a
	// malicious caller cannot bind their file row to someone else's
	// upload.
	other := uuid.NewString()
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
		map[string]any{
			"file_id":    uploadResp.UploadID.String(),
			"object_key": tok.WorkspaceID + "/" + other + "/" + uuid.NewString(),
			"size_bytes": 1024,
		})
	if status == http.StatusOK {
		t.Errorf("foreign object_key confirm should fail, got 200")
	}

	// Upload-url for an unmapped room is a 404, not a 500.
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/attachments/upload-url", tok.Token,
		map[string]any{
			"kchat_room_id": "no-such-room",
			"filename":      "foo.txt",
			"mime_type":     "text/plain",
			"size_bytes":    1,
		})
	if status != http.StatusNotFound {
		t.Errorf("unmapped room: expected 404, got %d", status)
	}
}
