package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestReUploadCreatesNewVersion confirms that a second upload against
// an existing file_id appends a new file_versions row and advances
// current_version_id, surfaced by GET /files/{id}/versions.
//
// The public API does not yet ship a "version-to-existing-file"
// upload flow, so this test reproduces the underlying invariant by
// driving confirm-upload twice with the same file_id (the second
// call gets a hand-crafted object_key inside the workspaceID/fileID
// prefix the handler signs against).
func TestReUploadCreatesNewVersion(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	wsID, _ := uuid.Parse(tok.WorkspaceID)
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	fileID := confirmUploadHelper(t, env, tok.Token, fold.ID, "memo.txt", "text/plain", 100)
	addAdditionalVersion(t, env, tok.Token, wsID, fileID, 200)

	status, body := env.httpRequest(http.MethodGet, "/api/files/"+fileID.String()+"/versions", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list versions: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Versions []struct {
			ID            uuid.UUID `json:"id"`
			VersionNumber int       `json:"version_number"`
			SizeBytes     int64     `json:"size_bytes"`
		} `json:"versions"`
	}
	env.decodeJSON(body, &resp)
	if len(resp.Versions) != 2 {
		t.Fatalf("expected 2 versions, got %d (%+v)", len(resp.Versions), resp.Versions)
	}
	// Versions ordered newest first; v2 should report the second
	// confirm's size.
	if resp.Versions[0].SizeBytes != 200 || resp.Versions[1].SizeBytes != 100 {
		t.Fatalf("unexpected version sizes: %+v", resp.Versions)
	}
}
