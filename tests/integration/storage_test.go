package integration

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestUploadURLGeneration verifies the presigned PUT URL flow returns a
// parseable URL that embeds AWS SigV4 query parameters. It runs without a
// live zk-object-fabric because presigned URL generation is a pure-local
// signing operation.
func TestUploadURLGeneration(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	status, body := env.httpRequest(http.MethodPost, "/api/files/upload-url", tok.Token, map[string]string{
		"folder_id": fold.ID.String(),
		"filename":  "report.pdf",
		"mime_type": "application/pdf",
	})
	if status != http.StatusOK {
		t.Fatalf("upload-url: status=%d body=%s", status, string(body))
	}

	var resp struct {
		UploadURL string    `json:"upload_url"`
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
	}
	env.decodeJSON(body, &resp)

	if resp.UploadURL == "" {
		t.Fatal("upload_url is empty")
	}
	if resp.UploadID == uuid.Nil {
		t.Fatal("upload_id is zero uuid")
	}
	if resp.ObjectKey == "" {
		t.Fatal("object_key is empty")
	}

	// The object key must carry the workspace and file prefix so the
	// gateway can enforce tenant isolation via a path-scoped policy.
	prefix := tok.WorkspaceID + "/" + resp.UploadID.String() + "/"
	if !strings.HasPrefix(resp.ObjectKey, prefix) {
		t.Errorf("object_key %q does not start with %q", resp.ObjectKey, prefix)
	}

	// Validate the signed URL looks like an AWS SigV4 presigned URL.
	parsed, err := url.Parse(resp.UploadURL)
	if err != nil {
		t.Fatalf("parse upload url: %v", err)
	}
	q := parsed.Query()
	if q.Get("X-Amz-Algorithm") == "" || q.Get("X-Amz-Signature") == "" || q.Get("X-Amz-Expires") == "" {
		t.Errorf("upload url missing SigV4 params: %q", resp.UploadURL)
	}
}

// TestUploadURLRejectsCrossWorkspaceFolder verifies a caller cannot request
// a presigned URL for a folder in a different workspace.
func TestUploadURLRejectsCrossWorkspaceFolder(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "pw2")
	aliceFolder := createFolder(t, env, alice.Token, nil, "Docs")

	status, _ := env.httpRequest(http.MethodPost, "/api/files/upload-url", bob.Token, map[string]string{
		"folder_id": aliceFolder.ID.String(),
		"filename":  "secret.pdf",
		"mime_type": "application/pdf",
	})
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for cross-tenant upload-url, got %d", status)
	}
}

// TestDownloadURL404WithoutVersion verifies the download endpoint returns
// 404 for a file that has never been confirmed (no current_version_id).
func TestDownloadURL404WithoutVersion(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "empty.bin", "application/octet-stream")

	status, body := env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String()+"/download-url", tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for file without version, got %d body=%s", status, string(body))
	}
}

// TestUploadConfirmDownloadRoundTrip exercises the full flow against a
// running zk-object-fabric gateway when S3_ENDPOINT is set. The test is
// skipped in CI where no gateway is available.
func TestUploadConfirmDownloadRoundTrip(t *testing.T) {
	if os.Getenv("S3_ENDPOINT") == "" {
		t.Skip("S3_ENDPOINT not set; skipping round-trip against zk-object-fabric")
	}

	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	// 1) Ask for a presigned PUT URL.
	status, body := env.httpRequest(http.MethodPost, "/api/files/upload-url", tok.Token, map[string]string{
		"folder_id": fold.ID.String(),
		"filename":  "hello.txt",
		"mime_type": "text/plain",
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

	// 2) PUT the bytes directly at the gateway.
	payload := []byte("hello zk-object-fabric\n")
	putReq, err := http.NewRequest(http.MethodPut, urlResp.UploadURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new put request: %v", err)
	}
	putReq.Header.Set("Content-Type", "text/plain")
	putReq.ContentLength = int64(len(payload))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("put upload: %v", err)
	}
	putBody, _ := io.ReadAll(putResp.Body)
	_ = putResp.Body.Close()
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		t.Fatalf("put upload: status=%d body=%s", putResp.StatusCode, string(putBody))
	}

	// 3) Confirm the upload.
	status, body = env.httpRequest(http.MethodPost, "/api/files/confirm-upload", tok.Token, map[string]any{
		"file_id":    urlResp.UploadID.String(),
		"object_key": urlResp.ObjectKey,
		"size_bytes": len(payload),
		"checksum":   "",
	})
	if status != http.StatusOK {
		t.Fatalf("confirm-upload: status=%d body=%s", status, string(body))
	}

	// 4) Request a download URL and fetch the bytes back.
	status, body = env.httpRequest(http.MethodGet, "/api/files/"+urlResp.UploadID.String()+"/download-url", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("download-url: status=%d body=%s", status, string(body))
	}
	var downloadResp struct {
		DownloadURL string `json:"download_url"`
		ObjectKey   string `json:"object_key"`
	}
	env.decodeJSON(body, &downloadResp)

	if downloadResp.ObjectKey != urlResp.ObjectKey {
		t.Errorf("object_key mismatch: %q vs %q", downloadResp.ObjectKey, urlResp.ObjectKey)
	}

	getResp, err := http.Get(downloadResp.DownloadURL)
	if err != nil {
		t.Fatalf("get download url: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get download url: status=%d", getResp.StatusCode)
	}
	got, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("read download body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("download mismatch: got %q want %q", string(got), string(payload))
	}
}
