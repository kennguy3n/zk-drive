package middleware_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-drive/api/middleware"
)

// echoHandler returns an http.Handler that fully drains the request
// body. It records the number of bytes read and any read error so the
// test can assert on the behaviour of the wrapped MaxBytesReader.
func echoHandler(t *testing.T, gotBytes *int, gotErr *error) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		*gotBytes = len(b)
		*gotErr = err
		if err != nil {
			// MaxBytesReader already calls
			// `w.WriteHeader(http.StatusRequestEntityTooLarge)`
			// via its ResponseController hook; here we just
			// surface the error message so the response body
			// is non-empty in oversize cases.
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestMaxBodySize(t *testing.T) {
	const limit = int64(64) // tiny limit to make oversize easy to construct

	tests := []struct {
		name        string
		method      string
		body        []byte
		wantStatus  int
		wantBytes   int
		wantMaxErr  bool
		description string
	}{
		{
			name:        "POST under limit succeeds",
			method:      http.MethodPost,
			body:        bytes.Repeat([]byte("a"), 32),
			wantStatus:  http.StatusOK,
			wantBytes:   32,
			description: "32 bytes < 64-byte cap; ReadAll returns the full payload",
		},
		{
			name:        "POST exactly at limit succeeds",
			method:      http.MethodPost,
			body:        bytes.Repeat([]byte("b"), 64),
			wantStatus:  http.StatusOK,
			wantBytes:   64,
			description: "MaxBytesReader allows reads up to and including the limit",
		},
		{
			name:        "POST over limit returns 413",
			method:      http.MethodPost,
			body:        bytes.Repeat([]byte("c"), 128),
			wantStatus:  http.StatusRequestEntityTooLarge,
			wantMaxErr:  true,
			description: "MaxBytesReader returns *http.MaxBytesError once the cap is exceeded",
		},
		{
			name:        "PUT over limit returns 413",
			method:      http.MethodPut,
			body:        bytes.Repeat([]byte("d"), 128),
			wantStatus:  http.StatusRequestEntityTooLarge,
			wantMaxErr:  true,
			description: "PUT is one of the mutating verbs the middleware caps",
		},
		{
			name:        "PATCH over limit returns 413",
			method:      http.MethodPatch,
			body:        bytes.Repeat([]byte("e"), 128),
			wantStatus:  http.StatusRequestEntityTooLarge,
			wantMaxErr:  true,
			description: "PATCH is one of the mutating verbs the middleware caps",
		},
		{
			name:        "GET with body is not capped",
			method:      http.MethodGet,
			body:        bytes.Repeat([]byte("f"), 128),
			wantStatus:  http.StatusOK,
			wantBytes:   128,
			description: "GET bodies are unusual; the middleware deliberately leaves them alone",
		},
		{
			name:        "DELETE with body is not capped",
			method:      http.MethodDelete,
			body:        bytes.Repeat([]byte("g"), 128),
			wantStatus:  http.StatusOK,
			wantBytes:   128,
			description: "DELETE is also exempt for the same reason as GET",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBytes int
			var gotErr error
			h := middleware.MaxBodySize(limit)(echoHandler(t, &gotBytes, &gotErr))

			req := httptest.NewRequest(tt.method, "/test", bytes.NewReader(tt.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, tt.description)
			}
			if tt.wantMaxErr {
				var mbe *http.MaxBytesError
				if !errors.As(gotErr, &mbe) {
					t.Fatalf("expected *http.MaxBytesError, got %T %v", gotErr, gotErr)
				}
				if mbe.Limit != limit {
					t.Fatalf("MaxBytesError.Limit = %d, want %d", mbe.Limit, limit)
				}
			} else {
				if gotErr != nil {
					t.Fatalf("unexpected read error: %v", gotErr)
				}
				if gotBytes != tt.wantBytes {
					t.Fatalf("read %d bytes, want %d", gotBytes, tt.wantBytes)
				}
			}
		})
	}
}

// TestMaxBodySize_NestedCapHonoured locks in the contract relied on by
// the Stripe webhook: a route that wraps r.Body a second time with a
// tighter MaxBytesReader must still trip on the inner cap, not the
// outer global one. If this regresses, the Stripe webhook's 64 KiB
// limit silently becomes whatever the global limit is.
func TestMaxBodySize_NestedCapHonoured(t *testing.T) {
	const (
		outerLimit = int64(1024) // 1 KiB outer / global
		innerLimit = int64(64)   // 64 B  inner / per-route
	)

	body := bytes.Repeat([]byte("x"), 128) // > inner, < outer

	var (
		gotBytes int
		gotErr   error
	)

	// Inner handler re-wraps the body with the tighter limit, the
	// same pattern internal/billing/stripe.go uses today.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, innerLimit)
		b, err := io.ReadAll(r.Body)
		gotBytes = len(b)
		gotErr = err
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	h := middleware.MaxBodySize(outerLimit)(inner)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 — inner 64 B cap should have tripped", rec.Code)
	}
	var mbe *http.MaxBytesError
	if !errors.As(gotErr, &mbe) {
		t.Fatalf("expected *http.MaxBytesError, got %T %v", gotErr, gotErr)
	}
	if mbe.Limit != innerLimit {
		t.Fatalf("MaxBytesError.Limit = %d, want %d (inner cap)", mbe.Limit, innerLimit)
	}
	if gotBytes >= int(outerLimit) {
		t.Fatalf("read %d bytes, expected the inner reader to abort before the outer cap", gotBytes)
	}
}

// TestMaxBodySize_ZeroOrNegativeNoLimit verifies the documented
// passthrough behaviour for non-positive limits — useful for tests
// that want to wire the middleware in but disable enforcement.
func TestMaxBodySize_ZeroOrNegativeNoLimit(t *testing.T) {
	for _, lim := range []int64{0, -1} {
		t.Run("limit_"+itoa(lim), func(t *testing.T) {
			var gotBytes int
			var gotErr error
			h := middleware.MaxBodySize(lim)(echoHandler(t, &gotBytes, &gotErr))

			payload := strings.Repeat("z", 4096)
			req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(payload))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if gotBytes != len(payload) {
				t.Fatalf("read %d bytes, want %d", gotBytes, len(payload))
			}
			if gotErr != nil {
				t.Fatalf("unexpected error: %v", gotErr)
			}
		})
	}
}

// itoa avoids pulling in strconv just for two test subtests.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
