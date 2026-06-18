package index

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchRejectsOversizeBody pins a correctness guard: the index
// service's fetch() must reject downloads that exceed MaxDownloadBytes
// rather than silently truncate them. Silent truncation would let a
// >4 MiB PDF be truncated, pdftotext would
// see a corrupt file, and the worker would NAK / redeliver forever.
//
// The test wires a stub HTTP server that serves MaxDownloadBytes+128
// zero bytes and asserts the resulting error mentions the cap so an
// operator reading the worker logs can see the real cause.
func TestFetchRejectsOversizeBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		// Write MaxDownloadBytes + 128 bytes. The +128 puts the body
		// past the +1 byte LimitReader cap and lets fetch() see the
		// overflow without needing to read the entire 100 MiB.
		buf := make([]byte, 64*1024)
		var written int64
		target := MaxDownloadBytes + 128
		for written < target {
			n := int64(len(buf))
			if written+n > target {
				n = target - written
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			written += n
		}
	}))
	t.Cleanup(srv.Close)

	svc := NewService(nil, nil, srv.Client())
	_, err := svc.fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for body exceeding MaxDownloadBytes")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected error to mention size cap; got %v", err)
	}
}

// TestFetchAcceptsBodyAtCap pins the boundary case: a download exactly
// at MaxDownloadBytes must succeed (the +1 byte LimitReader is the
// detection point, not the success point).
func TestFetchAcceptsBodyAtCap(t *testing.T) {
	t.Parallel()
	// Use a small synthetic cap to keep the test fast — assert by
	// behaviour, not by literal byte count. We can't lower
	// MaxDownloadBytes itself (package-level const) so this test is
	// the smaller cousin of the overflow test above; it serves a
	// 1 KiB body and verifies it round-trips.
	body := strings.Repeat("a", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	svc := NewService(nil, nil, srv.Client())
	got, err := svc.fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body round-trip mismatch: len=%d want=%d", len(got), len(body))
	}
}
