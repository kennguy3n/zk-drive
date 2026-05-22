package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestConfirmUploadRejectsTraversalKeys is the end-to-end half of WS-3:
// the storage.ValidateObjectKey unit tests (internal/storage) cover
// every input class, this test verifies the HTTP handler actually
// routes a tampered object_key through that validator and returns 403
// before any FileVersion is written.
//
// We don't need a live zk-object-fabric for this — the validator runs
// before the storage client is touched.
func TestConfirmUploadRejectsTraversalKeys(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	// Obtain a real upload URL so the file row exists. The handler
	// looks up the file before validating the key, so we need a
	// legitimate file_id for every probe.
	status, body := env.httpRequest(http.MethodPost, "/api/files/upload-url", tok.Token, map[string]string{
		"folder_id": fold.ID.String(),
		"filename":  "report.pdf",
		"mime_type": "application/pdf",
	})
	if status != http.StatusOK {
		t.Fatalf("upload-url: status=%d body=%s", status, string(body))
	}
	var urlResp struct {
		UploadURL string    `json:"upload_url"`
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
	}
	env.decodeJSON(body, &urlResp)

	canonical := urlResp.ObjectKey
	if canonical == "" {
		t.Fatal("upload-url returned empty object_key")
	}
	// Sanity check on the format we are about to mutate.
	parts := strings.Split(canonical, "/")
	if len(parts) != 3 {
		t.Fatalf("expected 3-segment canonical key, got %q", canonical)
	}
	wsSeg, fileSeg, verSeg := parts[0], parts[1], parts[2]

	tests := []struct {
		name       string
		objectKey  string
		wantStatus int
	}{
		{
			name:       "canonical key accepted (control)",
			objectKey:  canonical,
			wantStatus: http.StatusOK,
		},
		{
			name:       "trailing dotdot traversal rejected",
			objectKey:  canonical + "/../../etc/passwd",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "dotdot in middle segment rejected (HasPrefix bypass)",
			objectKey:  wsSeg + "/../" + fileSeg + "/" + verSeg,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "single dot final segment rejected",
			objectKey:  wsSeg + "/" + fileSeg + "/.",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "null byte in key rejected",
			objectKey:  canonical + "\x00.txt",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "backslash separators rejected",
			objectKey:  wsSeg + "\\" + fileSeg + "\\" + verSeg,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "non-UUID version segment rejected",
			objectKey:  wsSeg + "/" + fileSeg + "/not-a-uuid",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross-workspace prefix rejected",
			objectKey:  uuid.NewString() + "/" + fileSeg + "/" + verSeg,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross-file prefix rejected (same workspace)",
			objectKey:  wsSeg + "/" + uuid.NewString() + "/" + verSeg,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "extra trailing segment rejected",
			objectKey:  canonical + "/extra",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := env.httpRequest(http.MethodPost, "/api/files/confirm-upload", tok.Token, map[string]any{
				"file_id":    urlResp.UploadID.String(),
				"object_key": tt.objectKey,
				"size_bytes": 42,
				"checksum":   "",
			})
			if status != tt.wantStatus {
				t.Fatalf("confirm-upload status = %d, want %d body=%s", status, tt.wantStatus, string(body))
			}
		})
	}
}
