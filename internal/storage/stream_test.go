package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient builds a Client whose presigned URLs target srvURL, so
// PutObjectStream's raw PUT lands on a local httptest server we control.
func newTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c, err := NewClient(Config{
		Endpoint:  srvURL,
		Bucket:    "test-bucket",
		AccessKey: "test-access",
		SecretKey: "test-secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestPutObjectStreamStreamsBody is the core 5.5 guarantee: the body is
// relayed to the gateway with the exact Content-Length and Content-Type
// and the bytes arrive intact, via a single PUT minted from a presigned
// URL (no SDK payload buffering).
func TestPutObjectStreamStreamsBody(t *testing.T) {
	payload := bytes.Repeat([]byte("zk-drive-onlyoffice-stream"), 4096) // ~104 KiB

	var (
		gotMethod        string
		gotContentLength int64
		gotContentType   string
		gotPath          string
		gotBody          []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentLength = r.ContentLength
		gotContentType = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	const contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	err := c.PutObjectStream(context.Background(), "ws/file/version.docx", contentType, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("PutObjectStream: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", gotMethod)
	}
	if gotContentLength != int64(len(payload)) {
		t.Fatalf("Content-Length = %d, want %d", gotContentLength, len(payload))
	}
	if gotContentType != contentType {
		t.Fatalf("Content-Type = %q, want %q", gotContentType, contentType)
	}
	if !strings.Contains(gotPath, "version.docx") {
		t.Fatalf("path = %q, want it to contain the object key", gotPath)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(gotBody), len(payload))
	}
}

// TestPutObjectStreamGatewayError surfaces a non-2xx gateway response as
// an error (so the ONLYOFFICE callback acks non-zero and the Document
// Server retries) rather than silently dropping the save.
func TestPutObjectStreamGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain so the client always sees a clean response.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("AccessDenied: quota exceeded"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.PutObjectStream(context.Background(), "ws/file/v.bin", "application/octet-stream", strings.NewReader("data"), 4)
	if err == nil {
		t.Fatal("expected error on non-2xx gateway response, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should include the gateway status, got: %v", err)
	}
}

// TestPutObjectStreamValidation pins the cheap guard rails.
func TestPutObjectStreamValidation(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:0")
	ctx := context.Background()

	if err := c.PutObjectStream(ctx, "  ", "text/plain", strings.NewReader("x"), 1); err == nil {
		t.Fatal("empty object key should error")
	}
	if err := c.PutObjectStream(ctx, "k", "text/plain", nil, 1); err == nil {
		t.Fatal("nil body should error")
	}
	if err := c.PutObjectStream(ctx, "k", "text/plain", strings.NewReader("x"), -1); err == nil {
		t.Fatal("negative size should error")
	}

	var nilClient *Client
	if err := nilClient.PutObjectStream(ctx, "k", "text/plain", strings.NewReader("x"), 1); err == nil {
		t.Fatal("nil client should error")
	}
}
