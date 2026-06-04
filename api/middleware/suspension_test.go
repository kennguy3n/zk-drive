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
	guard := SuspensionGuard(fakeSuspensionChecker{suspended: true, reason: "abuse"})

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
			SuspensionGuard(checker)(next).ServeHTTP(rec, req)
			if !called || rec.Code != http.StatusOK {
				t.Fatalf("expected request to pass through (called=%v, code=%d)", called, rec.Code)
			}
		})
	}
}
