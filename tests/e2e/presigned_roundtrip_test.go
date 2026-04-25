//go:build e2e
// +build e2e

// Package e2e contains end-to-end tests that talk to a fully running
// zk-drive server backed by a real zk-object-fabric gateway. These
// tests are gated behind the `e2e` build tag so the default
// `go test ./...` run does not require a live zk-object-fabric
// instance; opt in with `go test -tags e2e ./tests/e2e/...`.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPresignedRoundTrip exercises the full presigned PUT / GET flow
// against a running zk-drive server with S3_ENDPOINT pointed at a
// real zk-object-fabric Docker demo:
//
//  1. POST /api/auth/signup to create a workspace + admin user.
//  2. POST /api/folders to create a target folder.
//  3. POST /api/files/upload-url to mint a presigned PUT URL.
//  4. PUT the bytes directly at the gateway via the presigned URL.
//  5. POST /api/files/confirm-upload to record the version.
//  6. GET /api/files/{id}/download-url to mint a presigned GET URL.
//  7. GET the bytes back from the gateway.
//  8. Assert the downloaded bytes match the uploaded payload.
//
// Required environment variables:
//   - ZK_DRIVE_BASE_URL: base URL of the zk-drive HTTP API
//     (e.g. http://localhost:8080). When unset the test is skipped so
//     CI runs without a live stack pass cleanly.
//
// Optional:
//   - ZK_DRIVE_E2E_WORKSPACE: workspace name to use. Defaults to a
//     timestamp-suffixed string so the test can be re-run against a
//     long-lived demo without unique-constraint collisions.
//   - ZK_DRIVE_E2E_PASSWORD: signup password. Defaults to a fixed
//     value; not security-sensitive in a demo.
func TestPresignedRoundTrip(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("ZK_DRIVE_BASE_URL"), "/")
	if baseURL == "" {
		t.Skip("ZK_DRIVE_BASE_URL not set; skipping e2e presigned round-trip")
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	workspace := os.Getenv("ZK_DRIVE_E2E_WORKSPACE")
	if workspace == "" {
		workspace = "e2e-ws-" + suffix
	}
	password := os.Getenv("ZK_DRIVE_E2E_PASSWORD")
	if password == "" {
		password = "e2e-test-password"
	}
	email := "e2e-" + suffix + "@example.test"

	client := &http.Client{Timeout: 30 * time.Second}

	// 1) Signup.
	var signup struct {
		Token       string `json:"token"`
		UserID      string `json:"user_id"`
		WorkspaceID string `json:"workspace_id"`
		Role        string `json:"role"`
	}
	doJSON(t, client, http.MethodPost, baseURL+"/api/auth/signup", "", map[string]string{
		"workspace_name": workspace,
		"email":          email,
		"name":           "E2E Tester",
		"password":       password,
	}, &signup)
	if signup.Token == "" {
		t.Fatalf("signup returned empty token")
	}

	// 2) Create a folder under the workspace root.
	var folder struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	doJSON(t, client, http.MethodPost, baseURL+"/api/folders", signup.Token, map[string]any{
		"name":             "E2E " + suffix,
		"parent_folder_id": nil,
	}, &folder)
	if folder.ID == "" {
		t.Fatalf("create folder returned empty id")
	}

	// 3) Mint a presigned PUT URL.
	payload := []byte("hello presigned round-trip\n")
	var uploadResp struct {
		UploadURL string `json:"upload_url"`
		UploadID  string `json:"upload_id"`
		ObjectKey string `json:"object_key"`
	}
	doJSON(t, client, http.MethodPost, baseURL+"/api/files/upload-url", signup.Token, map[string]string{
		"folder_id": folder.ID,
		"filename":  "hello.txt",
		"mime_type": "text/plain",
	}, &uploadResp)

	// Sanity-check the URL carries SigV4 query parameters; without
	// these the presigned signature would not be enforceable by the
	// gateway and the test would silently degrade to "PUT without
	// auth".
	parsed, err := url.Parse(uploadResp.UploadURL)
	if err != nil {
		t.Fatalf("parse upload url: %v", err)
	}
	q := parsed.Query()
	for _, k := range []string{"X-Amz-Algorithm", "X-Amz-Signature", "X-Amz-Expires", "X-Amz-Credential"} {
		if q.Get(k) == "" {
			t.Fatalf("upload url missing %s: %q", k, uploadResp.UploadURL)
		}
	}

	// 4) PUT the bytes through the presigned URL. The HTTP client
	// must send the body with the same Content-Type the SDK signed
	// or zk-object-fabric will reject the canonical request hash.
	putReq, err := http.NewRequest(http.MethodPut, uploadResp.UploadURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	putReq.Header.Set("Content-Type", "text/plain")
	putReq.ContentLength = int64(len(payload))
	putResp, err := client.Do(putReq)
	if err != nil {
		t.Fatalf("PUT presigned: %v", err)
	}
	putBody, _ := io.ReadAll(putResp.Body)
	_ = putResp.Body.Close()
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		t.Fatalf("PUT presigned: status=%d body=%s", putResp.StatusCode, string(putBody))
	}

	// 5) Confirm the upload so zk-drive records the version.
	doJSON(t, client, http.MethodPost, baseURL+"/api/files/confirm-upload", signup.Token, map[string]any{
		"file_id":    uploadResp.UploadID,
		"object_key": uploadResp.ObjectKey,
		"size_bytes": len(payload),
		"checksum":   "",
	}, nil)

	// 6) Mint a presigned GET URL.
	var downloadResp struct {
		DownloadURL string `json:"download_url"`
		ObjectKey   string `json:"object_key"`
	}
	doJSON(t, client, http.MethodGet, baseURL+"/api/files/"+uploadResp.UploadID+"/download-url", signup.Token, nil, &downloadResp)
	if downloadResp.ObjectKey != uploadResp.ObjectKey {
		t.Fatalf("download object_key %q != upload object_key %q", downloadResp.ObjectKey, uploadResp.ObjectKey)
	}

	// 7) GET the bytes back without sending an Authorization header
	// — the presigned URL carries SigV4 in the query string, which
	// is exactly what the upstream PresignedV4Strategy validates.
	getResp, err := client.Get(downloadResp.DownloadURL)
	if err != nil {
		t.Fatalf("GET presigned: %v", err)
	}
	got, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET presigned: status=%d body=%s", getResp.StatusCode, string(got))
	}

	// 8) Assert the downloaded bytes match the uploaded payload.
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded bytes do not match uploaded payload\n got:  %q\n want: %q", string(got), string(payload))
	}
}

// doJSON sends a JSON request and decodes the JSON response body into
// out (when non-nil). It fatals on transport errors or on non-2xx
// responses so test bodies stay readable.
func doJSON(t *testing.T, client *http.Client, method, fullURL, token string, in any, out any) {
	t.Helper()
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, fullURL, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s: status=%d body=%s", method, fullURL, resp.StatusCode, string(respBody))
	}
	if out == nil || len(respBody) == 0 {
		return
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		t.Fatalf("decode %s %s response: %v body=%s", method, fullURL, err, string(respBody))
	}
}

