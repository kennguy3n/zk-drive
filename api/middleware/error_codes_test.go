package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRespondError_JSONShape verifies the JSON contract that the
// frontend's api/errors.ts depends on: a body with {code, message}
// and an application/json Content-Type. Renaming the fields, or
// switching back to plain-text error bodies, would break every
// translated error message in the UI — pin the shape here.
func TestRespondError_JSONShape(t *testing.T) {
	rec := httptest.NewRecorder()
	RespondError(rec, http.StatusForbidden, ErrCodeAdminOnly, "admin required")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusForbidden)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type: got %q, want application/json; charset=utf-8", ct)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (body=%q)", err, rec.Body.String())
	}
	if resp.Code != ErrCodeAdminOnly {
		t.Fatalf("code: got %q, want %q", resp.Code, ErrCodeAdminOnly)
	}
	if resp.Message != "admin required" {
		t.Fatalf("message: got %q, want %q", resp.Message, "admin required")
	}
	if resp.Details != nil {
		t.Fatalf("details: got %v, want nil for RespondError", resp.Details)
	}
}

// TestRespondError_NoSniffHeader verifies the defense-in-depth
// X-Content-Type-Options=nosniff header is set on every error
// response. This prevents a browser from reinterpreting an error
// body as HTML/JS in a cross-origin or sniffing-attack scenario.
func TestRespondError_NoSniffHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	RespondError(rec, http.StatusBadRequest, ErrCodeBadRequest, "nope")

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options: got %q, want %q", got, "nosniff")
	}
}

// TestRespondValidationError_Details verifies the field-error map
// is serialised under `details` so the frontend can attach errors
// to individual form fields rather than only showing one toast.
func TestRespondValidationError_Details(t *testing.T) {
	rec := httptest.NewRecorder()
	RespondValidationError(rec, "bad", map[string]string{
		"name":     "REQUIRED",
		"password": "TOO_SHORT",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if resp.Code != ErrCodeValidation {
		t.Fatalf("code: got %q, want %q", resp.Code, ErrCodeValidation)
	}
	if got, want := resp.Details["name"], "REQUIRED"; got != want {
		t.Fatalf("details[name]: got %q, want %q", got, want)
	}
	if got, want := resp.Details["password"], "TOO_SHORT"; got != want {
		t.Fatalf("details[password]: got %q, want %q", got, want)
	}
}

// TestErrorCodes_DistinctValues guards against the easy mistake
// of declaring two ErrorCode constants with the same string value
// (which would silently collapse two different error conditions
// into one translation key on the frontend).
func TestErrorCodes_DistinctValues(t *testing.T) {
	codes := []ErrorCode{
		ErrCodeAuthMissingToken,
		ErrCodeAuthInvalidToken,
		ErrCodeAuthRevokedToken,
		ErrCodeAuthBadPurpose,
		ErrCodeAuthMissingIat,
		ErrCodeRevocationCheck,
		ErrCodeMFARequired,
		ErrCodeMFAInvalid,
		ErrCodeMFAEnrollNeeded,
		ErrCodeForbidden,
		ErrCodeAdminOnly,
		ErrCodeReadOnly,
		ErrCodeWrongTenant,
		ErrCodeNoWorkspace,
		ErrCodeRateLimit,
		ErrCodeValidation,
		ErrCodeBadRequest,
		ErrCodeMalformedJSON,
		ErrCodeMissingField,
		ErrCodeUnsupportedOp,
		ErrCodeCollabModeNotAllowed,
		ErrCodeNotFound,
		ErrCodeConflict,
		ErrCodeGone,
		ErrCodeFolderLocked,
		ErrCodeQuotaExceeded,
		ErrCodeFileTooLarge,
		ErrCodeVirusDetected,
		ErrCodeSharePasswordRequired,
		ErrCodeBillingNotConfigured,
		ErrCodeInternal,
		ErrCodeUpstream,
		ErrCodeMaintenance,
		ErrCodeStorageFailure,
	}
	seen := make(map[ErrorCode]int, len(codes))
	for i, c := range codes {
		if prev, dup := seen[c]; dup {
			t.Errorf("duplicate code %q at indexes %d and %d", c, prev, i)
		}
		seen[c] = i
	}
}
