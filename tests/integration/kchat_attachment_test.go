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
	// upload. The mismatch is a client error (400), not a 500.
	other := uuid.NewString()
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
		map[string]any{
			"file_id":    uploadResp.UploadID.String(),
			"object_key": tok.WorkspaceID + "/" + other + "/" + uuid.NewString(),
			"size_bytes": 1024,
		})
	if status != http.StatusBadRequest {
		t.Errorf("foreign object_key confirm: expected 400, got %d", status)
	}

	// Confirming with an unknown file_id is a 404, not a 500. The
	// service surfaces file.ErrNotFound and the handler maps it.
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
		map[string]any{
			"file_id":    uuid.NewString(),
			"object_key": uploadResp.ObjectKey,
			"size_bytes": 1024,
		})
	if status != http.StatusNotFound {
		t.Errorf("unknown file_id confirm: expected 404, got %d", status)
	}

	// Empty object_key is a 400 (kchat.ErrInvalidObjectKey).
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
		map[string]any{
			"file_id":    uploadResp.UploadID.String(),
			"object_key": "   ",
			"size_bytes": 1024,
		})
	if status != http.StatusBadRequest {
		t.Errorf("empty object_key confirm: expected 400, got %d", status)
	}

	// Negative size_bytes is a 400 (kchat.ErrInvalidSize).
	status, _ = env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
		map[string]any{
			"file_id":    uploadResp.UploadID.String(),
			"object_key": uploadResp.ObjectKey,
			"size_bytes": -1,
		})
	if status != http.StatusBadRequest {
		t.Errorf("negative size_bytes confirm: expected 400, got %d", status)
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

// TestKChatAttachmentConfirmRejectsTraversalKeys exercises the
// path-traversal defence on the KChat attachment confirm endpoint.
// Before the canonical-form validator the kchat service used a `strings.HasPrefix` check
// matching the bug in the main drive ConfirmUpload handler: a key
// like `<workspace>/<file>/../../other-tenant/secret` satisfied the
// prefix yet still resolved to a foreign S3 object once a presigned
// URL was generated for it. The fix routes the validation through
// the same `storage.ValidateObjectKey` (via an injected
// `ObjectKeyValidator` to preserve kchat's package independence)
// so both code paths share one canonical-form enforcement.
//
// We assert each adversarial shape returns 400 (the kchat handler's
// ErrObjectKeyMismatch mapping) and never lets the request reach
// the file repository.
func TestKChatAttachmentConfirmRejectsTraversalKeys(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Pen", "pen@example.com", "Pen", "hunter2hunter2")

	const roomID = "kchat-att-traversal"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": roomID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", status, string(body))
	}

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/attachments/upload-url", tok.Token,
		map[string]any{
			"kchat_room_id": roomID,
			"filename":      "secret.pdf",
			"mime_type":     "application/pdf",
			"size_bytes":    1024,
		})
	if status != http.StatusOK {
		t.Fatalf("upload-url: status=%d body=%s", status, string(body))
	}
	var u struct {
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
	}
	env.decodeJSON(body, &u)

	// All four shapes share the same prefix as the canonical key
	// minted above (workspace_uuid + "/" + file_uuid + "/") so a
	// naive HasPrefix check would let them through. The canonical
	// validator rejects each one.
	canonicalPrefix := tok.WorkspaceID + "/" + u.UploadID.String() + "/"
	cases := []struct {
		name string
		key  string
	}{
		{"trailing_dotdot_traversal", canonicalPrefix + uuid.NewString() + "/../../other-tenant/secret"},
		{"dotdot_segment_after_prefix", canonicalPrefix + "../other-tenant/secret"},
		{"null_byte_in_version_segment", canonicalPrefix + uuid.NewString() + "\x00.bak"},
		{"backslash_separator_after_prefix", canonicalPrefix + uuid.NewString() + "\\..\\other"},
		{"non_uuid_version_segment", canonicalPrefix + "not-a-uuid"},
		{"extra_trailing_segment", canonicalPrefix + uuid.NewString() + "/extra"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, b := env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
				map[string]any{
					"file_id":    u.UploadID.String(),
					"object_key": c.key,
					"size_bytes": 1024,
				})
			if s != http.StatusBadRequest {
				t.Fatalf("forged key %q: expected 400, got %d body=%s", c.key, s, string(b))
			}
		})
	}

	// Sanity check: the canonical key still confirms fine — we
	// didn't accidentally break the happy path.
	s, b := env.httpRequest(http.MethodPost, "/api/kchat/attachments/confirm", tok.Token,
		map[string]any{
			"file_id":    u.UploadID.String(),
			"object_key": u.ObjectKey,
			"checksum":   "sha256:abcd",
			"size_bytes": 1024,
		})
	if s != http.StatusOK {
		t.Fatalf("canonical confirm regressed: status=%d body=%s", s, string(b))
	}
}
