package middleware

import (
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/zk-drive/internal/logging"
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
	//
	// Three distinct semantic groups, all 401, that the frontend
	// interceptor needs to distinguish:
	//
	//  - Token / session problems (AuthMissingToken, AuthInvalidToken,
	//    AuthRevokedToken, AuthBadPurpose, AuthMissingIat,
	//    RevocationCheck) — the JWT itself is dead or wrong-purpose;
	//    clearing localStorage and bouncing to /login is correct.
	//  - Login-flow wrong-credentials (AuthInvalidCredentials) — the
	//    user has no valid session yet; the form's catch block
	//    handles the error. Locale copy: "Email or password is
	//    incorrect", NOT "Your session has expired".
	//  - Mid-session step-up failures (AuthPasswordReverifyFailed,
	//    MFAInvalid) — the user IS authenticated; they typed the
	//    wrong password on a sensitive action (disable-2FA, change
	//    email) or the wrong 6-digit MFA code. Must NOT nuke the
	//    session. See frontend/src/api/client.ts
	//    NON_SESSION_401_CODES for the codes the interceptor treats
	//    as soft 401s.
	ErrCodeAuthMissingToken        ErrorCode = "AUTH_MISSING_TOKEN"
	ErrCodeAuthInvalidToken        ErrorCode = "AUTH_INVALID_TOKEN"
	ErrCodeAuthRevokedToken        ErrorCode = "AUTH_REVOKED_TOKEN"
	ErrCodeAuthBadPurpose          ErrorCode = "AUTH_BAD_PURPOSE"
	ErrCodeAuthMissingIat          ErrorCode = "AUTH_MISSING_IAT"
	ErrCodeRevocationCheck         ErrorCode = "AUTH_REVOCATION_CHECK_FAILED"
	ErrCodeAuthInvalidCredentials  ErrorCode = "AUTH_INVALID_CREDENTIALS"
	ErrCodeAuthPasswordReverify    ErrorCode = "AUTH_PASSWORD_REVERIFY_FAILED"
	ErrCodeMFARequired             ErrorCode = "AUTH_MFA_REQUIRED"
	ErrCodeMFAInvalid              ErrorCode = "AUTH_MFA_INVALID"
	ErrCodeMFAEnrollNeeded         ErrorCode = "MFA_ENROLL_REQUIRED"

	// Authorization failures (403 Forbidden).
	ErrCodeForbidden   ErrorCode = "FORBIDDEN"
	ErrCodeAdminOnly   ErrorCode = "ADMIN_ACCESS_REQUIRED"
	ErrCodeReadOnly    ErrorCode = "READ_ONLY_ROLE"
	ErrCodeWrongTenant ErrorCode = "WRONG_TENANT"

	// Workspace-routing failure (401 Unauthorized). Distinct from the
	// AUTH_* codes above because the user IS authenticated — we just
	// can't route the request to a specific workspace. Lives in its
	// own block (rather than under the 403 group above) because
	// api/middleware/tenant.go returns it with StatusUnauthorized,
	// matching the pre-refactor behavior of the legacy plain-text
	// `http.Error(..., StatusUnauthorized)` call at the same site.
	// Treating an unroutable request as 401 nudges clients to retry
	// with a workspace selector (see frontend/src/api/client.ts
	// interceptor) rather than show a "permission denied" screen,
	// which is the correct UX for the missing-header case.
	// Devin Review ANALYSIS_0002 on commit c964e26 flagged the
	// earlier placement under the 403 group as a comment/status
	// mismatch.
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

	// Workspace storage not yet provisioned with the fabric (404).
	// Distinct from NOT_FOUND because the admin UI needs to render
	// remediation copy ("contact your administrator to provision
	// storage") rather than a generic "resource not found" message.
	// Distinct from UNSUPPORTED_OPERATION because the operation IS
	// supported by this deployment — the *workspace* just hasn't
	// been wired up to the storage fabric yet, which is a
	// recoverable state. Devin Review BUG_0001 on commit 4f3b458
	// caught the prior misuse of UNSUPPORTED_OPERATION at four
	// admin handler call sites (GetPlacement, UpdateMFAPolicy
	// adjacent paths, GetCMK, UpdateCMK), where the user-facing
	// copy "This operation is not supported" misled admins into
	// believing the feature didn't exist on the deployment.
	ErrCodeFabricNotProvisioned ErrorCode = "FABRIC_NOT_PROVISIONED"

	// Share-link auth (401 / 403). Distinct from session auth so the
	// frontend can render a password prompt rather than a sign-in
	// screen — see api/drive/sharing.go writeSharingError.
	ErrCodeSharePasswordRequired ErrorCode = "SHARE_PASSWORD_REQUIRED"
	// Share-link download cap reached (429). Distinct from
	// RATE_LIMIT_EXCEEDED — rate-limit is a transient per-user
	// throttle ("retry later" makes sense) but link-exhaustion is
	// permanent (the link's download budget is used up; retrying
	// will not help). The frontend renders different copy and
	// different remediation guidance for the two cases.
	ErrCodeShareLinkExhausted ErrorCode = "SHARE_LINK_EXHAUSTED"

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
	WriteJSON(w, status, ErrorResponse{
		Code:    code,
		Message: message,
		Details: details,
	})
}

// RespondInternalError logs the underlying error to the request-
// scoped structured logger and writes a SANITISED 500 response.
// The op parameter is a stable, sanitised description of what
// the handler was doing ("list audit", "create checkout
// session") — it appears in both the log line and the response
// `message` field. The err is recorded in logs but NEVER
// included in the response, preventing internal details (DB
// driver state, filesystem paths, SQL fragments, third-party
// service identifiers, stack frame hints) from leaking to
// callers — particularly important on routes where the frontend
// is not the only consumer (CLI tools, future SDKs, log
// shippers that surface response bodies to dashboards).
//
// Before this helper, the codebase-wide pattern was
//
//	middleware.RespondError(w, http.StatusInternalServerError,
//	    middleware.ErrCodeInternal, "list audit: "+err.Error())
//
// which embedded raw err.Error() in the JSON body. The
// frontend's translateApiError correctly hides it (translates
// INTERNAL_ERROR → "Something went wrong on our end"), but the
// JSON `message` field still carried the leak for any other
// consumer that read the response body. Devin Review BUG on PR
// #83 commit 97679c2 flagged the pattern. The fix is two-fold:
// log the err server-side with full context (request_id,
// workspace_id, user_id, op, err.Error()), and respond with
// just the op — operators get the diagnostic, clients get a
// clean envelope.
//
// Call sites should migrate from RespondError + ErrCodeInternal
// to this helper for any 500-class internal failure. The
// helper deliberately takes *http.Request rather than a context,
// so the call site is identical to RespondError (`w, r, ...`)
// and a future audit can grep for the pattern.
func RespondInternalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	logger := logging.FromContext(r.Context())
	// log.Error level — these are 500s, so they always merit
	// alert-eligible severity even if the underlying err is
	// "context canceled" (operator can still diagnose canceled-
	// during-shutdown patterns from the log volume).
	logger.Error("internal error",
		"op", op,
		"err", err,
		"path", r.URL.Path,
		"method", r.Method,
	)
	WriteJSON(w, http.StatusInternalServerError, ErrorResponse{
		Code:    ErrCodeInternal,
		Message: op,
	})
}

// RespondUpstreamError is the 502 Bad Gateway analogue of
// RespondInternalError: the same redaction contract, but for the
// case where this handler reached out to a downstream service
// (object storage fabric, billing provider, identity provider) and
// the downstream returned an error or timed out. Operators still
// get the raw err in the slog logger; clients see only the
// stable op label in the JSON `message` field.
//
// Why this is a separate helper rather than a status param on
// RespondInternalError: 5xx vs upstream-5xx is a meaningful
// distinction for SRE dashboards and for the frontend, which
// translates UPSTREAM_FAILED → "An upstream service didn't
// respond" (transient, retryable) and INTERNAL_ERROR → "Something
// went wrong on our end" (escalate). Naming the helper at the
// call site preserves that signal.
//
// Devin Review ANALYSIS_0007 on commit a2e52fb noted that admin
// handlers were dropping raw err.Error() into 502 responses
// ("get placement: " + err.Error(), "put placement: " + err.Error()),
// which leaked fabric provider URLs / driver state / endpoint
// hints. The helper plugs that leak with the same machinery as
// RespondInternalError.
func RespondUpstreamError(w http.ResponseWriter, r *http.Request, op string, err error) {
	logger := logging.FromContext(r.Context())
	logger.Error("upstream error",
		"op", op,
		"err", err,
		"path", r.URL.Path,
		"method", r.Method,
	)
	WriteJSON(w, http.StatusBadGateway, ErrorResponse{
		Code:    ErrCodeUpstream,
		Message: op,
	})
}

// WriteJSON writes a JSON-encoded payload with the canonical
// response headers used across every handler package. Prefer this
// over hand-rolled `w.Header().Set("Content-Type", "application/json")`
// + `json.NewEncoder(w).Encode(...)` so success responses share
// the same Content-Type charset and the same X-Content-Type-Options
// defence as error responses written through RespondError. Earlier
// versions of the code had each handler package (api/auth,
// api/admin, api/webhooks, api/drive, api/kchat) carry its own
// writeJSON helper, and the headers drifted apart over time —
// drive/helpers.go.writeJSON omitted charset=utf-8 and nosniff,
// while error responses going through the same handler set both.
// Consolidating here keeps a single source of truth.
//
// payload is allowed to be nil; the body is then written as
// JSON `null` per encoding/json semantics. Callers that don't
// want a body should call w.WriteHeader directly instead.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Defense-in-depth: refuse content-type sniffing so a browser
	// that receives this body in an <img>/<script>/<iframe> context
	// (e.g. an authenticated cross-origin error page in an XSS
	// scenario) cannot reinterpret the bytes as something other
	// than the JSON we sent. Cheap, no downside, defends against
	// future MIME-confusion mistakes elsewhere on the response.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// Intentionally swallow the encoding error: the response has
	// already been committed (WriteHeader fired), so there is no
	// recovery path. The encoder's only failure modes here are a
	// closed connection (caller already gone) or buffer exhaustion
	// (process is in worse trouble than a missing body).
	_ = json.NewEncoder(w).Encode(payload)
}
