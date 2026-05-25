package middleware

import (
	"encoding/json"
	"net/http"
)

// ErrorCode is a stable, locale-independent identifier for an
// error condition the API can return. Frontends map these to
// translated user-facing copy via locale JSON files (see
// frontend/src/i18n/locales/en.json `errors` namespace).
//
// Contract:
//   - Codes are SCREAMING_SNAKE_CASE.
//   - Codes are append-only — never rename or repurpose. Adding
//     a new error condition means adding a new code; deleting an
//     error path requires keeping the old code around until every
//     consumer (web, desktop, mobile, CLI) drops it.
//   - The frontend uses the code as a translation key; the
//     server-supplied `message` field is a developer-readable
//     English fallback that the client surfaces ONLY if the code
//     is unknown to the active locale (deploy skew).
//   - Status codes are paired with each code below. Callers that
//     use respondError get the mapping for free.
type ErrorCode string

const (
	// Authentication failures (401 Unauthorized).
	ErrCodeAuthMissingToken ErrorCode = "AUTH_MISSING_TOKEN"
	ErrCodeAuthInvalidToken ErrorCode = "AUTH_INVALID_TOKEN"
	ErrCodeAuthRevokedToken ErrorCode = "AUTH_REVOKED_TOKEN"
	ErrCodeAuthBadPurpose   ErrorCode = "AUTH_BAD_PURPOSE"
	ErrCodeAuthMissingIat   ErrorCode = "AUTH_MISSING_IAT"
	ErrCodeRevocationCheck  ErrorCode = "AUTH_REVOCATION_CHECK_FAILED"
	ErrCodeMFARequired      ErrorCode = "AUTH_MFA_REQUIRED"
	ErrCodeMFAInvalid       ErrorCode = "AUTH_MFA_INVALID"
	ErrCodeMFAEnrollNeeded  ErrorCode = "MFA_ENROLL_REQUIRED"

	// Authorization failures (403 Forbidden).
	ErrCodeForbidden   ErrorCode = "FORBIDDEN"
	ErrCodeAdminOnly   ErrorCode = "ADMIN_ACCESS_REQUIRED"
	ErrCodeReadOnly    ErrorCode = "READ_ONLY_ROLE"
	ErrCodeWrongTenant ErrorCode = "WRONG_TENANT"
	ErrCodeNoWorkspace ErrorCode = "MISSING_WORKSPACE_CONTEXT"

	// Rate limiting (429 Too Many Requests).
	ErrCodeRateLimit ErrorCode = "RATE_LIMIT_EXCEEDED"

	// Validation (400 / 422 Bad Request / Unprocessable Entity).
	ErrCodeValidation           ErrorCode = "VALIDATION_FAILED"
	ErrCodeBadRequest           ErrorCode = "BAD_REQUEST"
	ErrCodeMalformedJSON        ErrorCode = "MALFORMED_JSON"
	ErrCodeMissingField         ErrorCode = "MISSING_REQUIRED_FIELD"
	ErrCodeUnsupportedOp        ErrorCode = "UNSUPPORTED_OPERATION"
	ErrCodeCollabModeNotAllowed ErrorCode = "COLLAB_MODE_NOT_ALLOWED"

	// Resource state (404 / 409 / 410).
	ErrCodeNotFound      ErrorCode = "NOT_FOUND"
	ErrCodeConflict      ErrorCode = "CONFLICT"
	ErrCodeGone          ErrorCode = "GONE"
	ErrCodeFolderLocked  ErrorCode = "FOLDER_LOCKED"
	ErrCodeQuotaExceeded ErrorCode = "WORKSPACE_QUOTA_EXCEEDED"
	ErrCodeFileTooLarge  ErrorCode = "FILE_TOO_LARGE"
	ErrCodeVirusDetected ErrorCode = "FILE_VIRUS_DETECTED"

	// Share-link auth (401 / 403). Distinct from session auth so the
	// frontend can render a password prompt rather than a sign-in
	// screen — see api/drive/sharing.go writeSharingError.
	ErrCodeSharePasswordRequired ErrorCode = "SHARE_PASSWORD_REQUIRED"

	// Billing / payments (402 / 412). Distinct from internal so the
	// frontend can prompt for upgrade / customer-onboarding flows.
	ErrCodeBillingNotConfigured ErrorCode = "BILLING_NOT_CONFIGURED"

	// Service-level failures (5xx).
	ErrCodeInternal       ErrorCode = "INTERNAL_ERROR"
	ErrCodeUpstream       ErrorCode = "UPSTREAM_FAILED"
	ErrCodeMaintenance    ErrorCode = "MAINTENANCE"
	ErrCodeStorageFailure ErrorCode = "STORAGE_FAILURE"
)

// ErrorResponse is the canonical JSON body for every error the
// API returns through respondError. Keep this struct stable —
// the frontend's api/errors.ts depends on this exact shape.
//
// Optional `details` is used by validation handlers to surface
// field-level errors:
//
//	{
//	  "code": "VALIDATION_FAILED",
//	  "message": "name is required, password too short",
//	  "details": {"name": "REQUIRED", "password": "TOO_SHORT"}
//	}
type ErrorResponse struct {
	Code    ErrorCode         `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// RespondError writes a JSON ErrorResponse with the appropriate
// Content-Type header. Use this from handlers that need to
// surface a stable error code to the client. The `message`
// argument is the developer-readable English fallback — the
// frontend translates by code first and only uses message if the
// code is unknown to the active locale.
//
// Mirror calls to http.Error should be migrated to this helper
// as handlers are touched. Greenfield handlers should use this
// from day one.
func RespondError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	respondErrorWithDetails(w, status, code, message, nil)
}

// RespondValidationError writes a 400 with code=VALIDATION_FAILED
// and a `details` map listing the offending fields. Use from
// handlers that aggregate multiple validation errors so the
// frontend can surface them inline next to the form fields.
func RespondValidationError(w http.ResponseWriter, message string, details map[string]string) {
	respondErrorWithDetails(w, http.StatusBadRequest, ErrCodeValidation, message, details)
}

func respondErrorWithDetails(w http.ResponseWriter, status int, code ErrorCode, message string, details map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Defense-in-depth: refuse content-type sniffing so a browser
	// that receives this body in an <img>/<script>/<iframe> context
	// (e.g. an authenticated cross-origin error page in an XSS
	// scenario) cannot reinterpret the bytes as something other
	// than the JSON we sent. Cheap, no downside, defends against
	// future MIME-confusion mistakes elsewhere on the response.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// We intentionally swallow the encoding error: the response
	// has already been committed (WriteHeader fired), so there's
	// no recovery action available. The encoder's only failure
	// modes here are a closed connection (caller already gone) or
	// the encoder buffering ran out of memory (process is in
	// worse trouble than a missing error body).
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Code:    code,
		Message: message,
		Details: details,
	})
}
