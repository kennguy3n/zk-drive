package platform

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeOptional covers the optional-body decode used by
// SuspendWorkspace. A real body must always be decoded (even when the
// transport reports ContentLength == -1, as HTTP/2 and chunked requests
// do), an empty body must be tolerated as "no body", and a non-empty
// malformed body must still produce a 400.
func TestDecodeOptional(t *testing.T) {
	type body struct {
		Reason string `json:"reason"`
	}

	t.Run("valid body with unknown ContentLength is decoded", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"reason":"abuse"}`))
		// Simulate HTTP/2 / chunked: length unknown despite a real body.
		r.ContentLength = -1
		w := httptest.NewRecorder()

		var dst body
		if !decodeOptional(w, r, &dst) {
			t.Fatalf("expected decodeOptional to succeed, got status %d", w.Code)
		}
		if dst.Reason != "abuse" {
			t.Fatalf("body not decoded: %+v", dst)
		}
	})

	t.Run("empty body is tolerated", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
		r.ContentLength = -1
		w := httptest.NewRecorder()

		var dst body
		if !decodeOptional(w, r, &dst) {
			t.Fatalf("expected empty body to be tolerated, got status %d", w.Code)
		}
		if dst.Reason != "" {
			t.Fatalf("expected zero-value body, got %+v", dst)
		}
	})

	t.Run("malformed body is rejected with 400", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"reason":`))
		w := httptest.NewRecorder()

		var dst body
		if decodeOptional(w, r, &dst) {
			t.Fatalf("expected malformed body to be rejected")
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed body, got %d", w.Code)
		}
	})
}
