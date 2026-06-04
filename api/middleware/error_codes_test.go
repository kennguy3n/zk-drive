package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
		ErrCodeAuthInvalidCredentials,
		ErrCodeAuthPasswordReverify,
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
		ErrCodeUnsupportedLanguage,
		ErrCodeNotFound,
		ErrCodeConflict,
		ErrCodeGone,
		ErrCodeFolderLocked,
		ErrCodeQuotaExceeded,
		ErrCodeFileTooLarge,
		ErrCodeVirusDetected,
		ErrCodeFabricNotProvisioned,
		ErrCodeSharePasswordRequired,
		ErrCodeShareLinkExhausted,
		ErrCodeBillingNotConfigured,
		ErrCodeStripeNotConfigured,
		ErrCodeInternal,
		ErrCodeUpstream,
		ErrCodeMaintenance,
		ErrCodeStorageFailure,
		ErrCodeIPBlocked,
		ErrCodeInvalidCIDR,
		ErrCodePrivateCIDR,
		ErrCodeRuleCapExceeded,
		ErrCodeDuplicateCIDR,
		ErrCodeLabelTooLong,
		ErrCodeAllowlistNoRules,
		ErrCodeAllowlistLastRule,
	}
	seen := make(map[ErrorCode]int, len(codes))
	for i, c := range codes {
		if prev, dup := seen[c]; dup {
			t.Errorf("duplicate code %q at indexes %d and %d", c, prev, i)
		}
		seen[c] = i
	}
}

// TestRespondInternalError_RedactsErr verifies that the underlying
// err.Error() string is NEVER included in the JSON response body
// — only the sanitised op label is exposed to the client. The
// helper was introduced specifically to plug the err.Error() leak
// flagged by Devin Review on PR #83 commit 97679c2; this test
// ensures a future refactor can't accidentally reintroduce the
// leak. Run with `go test -run TestRespondInternalError_RedactsErr
// ./api/middleware` after any change to RespondInternalError.
func TestRespondInternalError_RedactsErr(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/audit", nil)
	const secretFragment = "pq: connect: connection refused (host=10.1.2.3)"
	RespondInternalError(rec, req, "list audit", errors.New(secretFragment))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	body := rec.Body.String()
	if strings.Contains(body, secretFragment) {
		t.Fatalf("response body leaked underlying error: %q", body)
	}
	if strings.Contains(body, "10.1.2.3") {
		t.Fatalf("response body leaked address fragment: %q", body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (body=%q)", err, body)
	}
	if resp.Code != ErrCodeInternal {
		t.Fatalf("code: got %q, want %q", resp.Code, ErrCodeInternal)
	}
	if resp.Message != "list audit" {
		t.Fatalf("message: got %q, want %q (op should be the only exposed string)", resp.Message, "list audit")
	}
}

// TestRespondUpstreamError_RedactsErr verifies the same redaction
// contract for the 502 Bad Gateway path. RespondUpstreamError is
// the wrapper for fabric / billing / identity-provider failures —
// admin handlers used to drop "get placement: " + err.Error() into
// the 502 message field, which could leak provider endpoint URLs,
// connection strings, or driver state. The helper takes the op
// label as the only client-exposed string.
func TestRespondUpstreamError_RedactsErr(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/placement", nil)
	const secretFragment = "fabric: dial https://internal-fabric.uney-poc.local:8443 connect: dial tcp 10.4.5.6:8443"
	RespondUpstreamError(rec, req, "get placement", errors.New(secretFragment))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadGateway)
	}
	body := rec.Body.String()
	if strings.Contains(body, secretFragment) {
		t.Fatalf("response body leaked upstream error: %q", body)
	}
	if strings.Contains(body, "10.4.5.6") || strings.Contains(body, "internal-fabric") {
		t.Fatalf("response body leaked upstream endpoint: %q", body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (body=%q)", err, body)
	}
	if resp.Code != ErrCodeUpstream {
		t.Fatalf("code: got %q, want %q", resp.Code, ErrCodeUpstream)
	}
	if resp.Message != "get placement" {
		t.Fatalf("message: got %q, want %q (op should be the only exposed string)", resp.Message, "get placement")
	}
}

// TestRespondInternalError_SetsCanonicalHeaders verifies that the
// helper goes through WriteJSON so internal-error 500 responses
// share the same Content-Type charset and X-Content-Type-Options
// defense as every other error response. Skipping these headers
// would make INTERNAL_ERROR responses subtly inconsistent with
// the rest of the error envelope and lose the MIME-confusion
// defense that the rest of the contract already has.
func TestRespondInternalError_SetsCanonicalHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/signup", nil)
	RespondInternalError(rec, req, "signup", errors.New("workspaces.Create: deadline exceeded"))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type: got %q, want application/json; charset=utf-8", ct)
	}
	if nosniff := rec.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("X-Content-Type-Options: got %q, want nosniff", nosniff)
	}
}
