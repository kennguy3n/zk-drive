package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// fakeSuspensionChecker stubs WorkspaceSuspensionChecker.
type fakeSuspensionChecker struct {
	suspended bool
	reason    string
	err       error
}

func (f fakeSuspensionChecker) WorkspaceSuspension(_ context.Context, _ uuid.UUID) (bool, string, error) {
	return f.suspended, f.reason, f.err
}

func TestSuspensionGuardBlocksSuspendedWorkspace(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next handler must not run for a suspended workspace")
	})
	guard := SuspensionGuard(fakeSuspensionChecker{suspended: true, reason: "abuse"}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/drive/files", nil).
		WithContext(WithWorkspaceID(context.Background(), uuid.New()))
	rec := httptest.NewRecorder()
	guard(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	// Must carry the same nosniff defense as every other JSON response.
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
	}
	var body suspendedWorkspaceBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "workspace_suspended" || body.Reason != "abuse" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestSuspensionGuardFailsOpen(t *testing.T) {
	// A lookup error and an active workspace both let the request through;
	// suspension is an availability control, not a security boundary.
	for name, checker := range map[string]fakeSuspensionChecker{
		"active":       {suspended: false},
		"lookup_error": {err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "/api/drive/files", nil).
				WithContext(WithWorkspaceID(context.Background(), uuid.New()))
			rec := httptest.NewRecorder()
			SuspensionGuard(checker, false)(next).ServeHTTP(rec, req)
			if !called || rec.Code != http.StatusOK {
				t.Fatalf("expected request to pass through (called=%v, code=%d)", called, rec.Code)
			}
		})
	}
}

func TestSuspensionGuardFailsClosed(t *testing.T) {
	// With failClosed=true a lookup error rejects the request with a
	// distinct "suspension_check_unavailable" body (not
	// "workspace_suspended"), so callers can tell "can't confirm" from a
	// confirmed suspension. An active workspace still passes through.
	t.Run("lookup_error_blocks", func(t *testing.T) {
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("next handler must not run when fail-closed and lookup errors")
		})
		req := httptest.NewRequest(http.MethodGet, "/api/drive/files", nil).
			WithContext(WithWorkspaceID(context.Background(), uuid.New()))
		rec := httptest.NewRecorder()
		SuspensionGuard(fakeSuspensionChecker{err: context.DeadlineExceeded}, true)(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status: got %d, want 503", rec.Code)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
		}
		var body suspendedWorkspaceBody
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Error != "suspension_check_unavailable" {
			t.Errorf("error: got %q, want suspension_check_unavailable", body.Error)
		}
	})
	t.Run("active_passes", func(t *testing.T) {
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/api/drive/files", nil).
			WithContext(WithWorkspaceID(context.Background(), uuid.New()))
		rec := httptest.NewRecorder()
		SuspensionGuard(fakeSuspensionChecker{suspended: false}, true)(next).ServeHTTP(rec, req)
		if !called || rec.Code != http.StatusOK {
			t.Fatalf("expected request to pass through (called=%v, code=%d)", called, rec.Code)
		}
	})
}
