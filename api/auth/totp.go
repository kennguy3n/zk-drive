package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/totp"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// TOTPHandler serves the /auth/totp/* endpoints. It depends on the
// underlying Handler so wiring stays centralised (jwtSecret, audit
// service, session revoker, user/workspace services, totp service)
// and the routes don't fan out their own per-endpoint constructors.
type TOTPHandler struct {
	h *Handler
}

// NewTOTPHandler returns a handler backed by the same dependencies
// as the surrounding auth handler. Returns nil when h.totp has not
// been wired — callers should refuse to mount the routes in that
// case rather than mounting handlers that always 500.
func NewTOTPHandler(h *Handler) *TOTPHandler {
	if h == nil || h.totp == nil {
		return nil
	}
	return &TOTPHandler{h: h}
}

type totpEnrollBeginResponse struct {
	Secret     string `json:"secret"`
	OtpauthURI string `json:"otpauth_uri"`
	// QRCodePNG is a base64 data:image/png;base64,... payload the
	// frontend can drop straight into <img src=...>. Keeps clients
	// from having to ship their own QR rendering library.
	QRCodePNG string `json:"qr_code_png"`
}

type totpFinalizeRequest struct {
	Code string `json:"code"`
}

type totpFinalizeResponse struct {
	// RecoveryCodes are the plaintext one-time recovery codes,
	// returned exactly once at finalize time. The client MUST
	// surface them to the user immediately (download / copy /
	// print). Server-side they exist only as bcrypt hashes from
	// this point forward.
	RecoveryCodes []string `json:"recovery_codes"`
}

type totpVerifyRequest struct {
	// Code accepts either a 6-digit TOTP value or a recovery code
	// (xb-4q-9z-pm-tk format, normalisation is lenient). The
	// handler tries TOTP first because the recovery-code path is
	// expensive (bcrypt cost 12 per stored hash).
	Code string `json:"code"`
}

type totpDisableRequest struct {
	// Password re-verify guard: a stolen session token cannot
	// disable 2FA without also knowing the password. This is the
	// industry-standard "re-authenticate for security-sensitive
	// changes" affordance (GitHub, Stripe, AWS console).
	Password string `json:"password"`
}

type totpStatusResponse struct {
	Enabled                bool       `json:"enabled"`
	PendingEnrollment      bool       `json:"pending_enrollment"`
	ActivatedAt            *time.Time `json:"activated_at,omitempty"`
	LastUsedAt             *time.Time `json:"last_used_at,omitempty"`
	RecoveryCodesRemaining int        `json:"recovery_codes_remaining"`
}

// EnrollBegin creates (or refreshes the pending row for) the
// caller's TOTP credential and returns the otpauth:// URI, base32
// secret, and QR-code PNG. Mounted behind AuthMiddleware OR
// PurposeMiddleware(PurposeMFAEnroll) — both reach the same handler
// because a user re-enrolling from a settings page and a user
// completing must-enroll on a workspace policy both need this
// endpoint.
//
// Refuses with 409 if the user already has an ACTIVATED credential.
// The user must Disable first (which itself requires a password
// re-verify) before re-enrolling. This is the lockout-prevention
// guarantee: a buggy frontend that re-runs Begin after activation
// cannot silently invalidate a working secret behind the user's
// back.
func (h *TOTPHandler) EnrollBegin(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}

	// The account label is what authenticator apps render under
	// the workspace name. We use the user's email so a user with
	// multiple workspaces can tell their 2FA entries apart at a
	// glance.
	u, err := h.h.users.GetByID(r.Context(), claims.WorkspaceID, claims.UserID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "lookup user: "+err.Error())
		return
	}
	challenge, err := h.h.totp.BeginEnrollment(r.Context(), claims.UserID, u.Email)
	if err != nil {
		if errors.Is(err, totp.ErrAlreadyActivated) {
			middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, "2FA already enrolled; disable first to re-enroll")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "begin enrollment: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, totpEnrollBeginResponse{
		Secret:     challenge.Secret,
		OtpauthURI: challenge.OtpauthURI,
		QRCodePNG:  challenge.QRCodePNG,
	})
}

// EnrollFinalize verifies a code against the pending secret and,
// on success, activates the credential and returns 10 plaintext
// recovery codes for the user to record.
//
// The recovery codes are returned exactly once: server-side they
// are bcrypt-hashed before commit and the plaintext is discarded.
// A client that loses the response body has no way to recover them
// short of Disable + re-enrollment (which generates a fresh set
// and invalidates the prior set).
func (h *TOTPHandler) EnrollFinalize(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	var req totpFinalizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "code is required")
		return
	}

	codes, err := h.h.totp.FinalizeEnrollment(r.Context(), claims.UserID, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, totp.ErrNotEnrolled):
			middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, "no pending enrollment")
		case errors.Is(err, totp.ErrAlreadyActivated):
			middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, "credential already activated")
		case errors.Is(err, totp.ErrInvalidCode):
			middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeMFAInvalid, "invalid code")
		default:
			middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "finalize: "+err.Error())
		}
		return
	}

	h.h.logAudit(r.Context(), claims.WorkspaceID, &claims.UserID, audit.ActionMFAEnroll, r, map[string]any{
		"result": "success",
	})

	writeJSON(w, http.StatusOK, totpFinalizeResponse{RecoveryCodes: codes})
}

// Verify accepts a mfa_challenge purpose token (from the login or
// OAuth callback fork) plus a one-time code (TOTP or recovery), and
// on success returns a full session JWT.
//
// The challenge token has already proven the password factor; this
// endpoint proves the possession factor. The two factors together
// are what mints the session.
//
// We try the TOTP path first because recovery codes are
// significantly more expensive (one bcrypt comparison per stored
// hash, vs the constant-time HMAC of TOTP). When TOTP rejects, we
// fall back to the recovery path. Both paths collapse onto
// "invalid code" with the same response shape so an attacker
// cannot tell whether a code matched a TOTP slot or a recovery
// slot from response timing alone.
func (h *TOTPHandler) Verify(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	if claims.Purpose != middleware.PurposeMFAChallenge {
		// Defence-in-depth: production routes Verify behind
		// PurposeMiddleware(PurposeMFAChallenge), so this
		// branch is unreachable. Belt-and-braces in case a
		// future router change accidentally drops the
		// middleware.
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthBadPurpose, "wrong token purpose")
		return
	}

	var req totpVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "code is required")
		return
	}

	// Look up the user once so we can reuse the row for both
	// verify and the success-path UpdateLastLogin / token issuance.
	u, err := h.h.users.GetByID(r.Context(), claims.WorkspaceID, claims.UserID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthInvalidToken, "invalid credentials")
			return
		}
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "lookup user: "+err.Error())
		return
	}
	if u.DeactivatedAt != nil {
		// A user deactivated between password-verify and TOTP-
		// verify must not be able to complete login. The audit
		// trail is best-effort.
		h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "deactivated_at_mfa",
		})
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, "account deactivated")
		return
	}

	verr := h.h.totp.Verify(r.Context(), claims.UserID, req.Code)
	usedRecovery := false
	if verr != nil {
		// Try the recovery-code path. ConsumeRecoveryCode does
		// its own well-formedness check and burns the matching
		// row in a predicate-guarded UPDATE so a concurrent
		// retry cannot reuse the same code.
		if rerr := h.h.totp.ConsumeRecoveryCode(r.Context(), claims.UserID, req.Code); rerr != nil {
			// Both factors rejected. Audit the failure so a
			// brute-force attempt is visible to operators, and
			// return a generic 401.
			h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionMFAVerify, r, map[string]any{
				"result": "invalid_code",
			})
			middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeMFAInvalid, "invalid code")
			return
		}
		usedRecovery = true
	}

	// Successful second factor. Stamp last_login_at now (we
	// deferred this from the password step so last_login_at
	// reflects the effective sign-in, not the partial one).
	if err := h.h.users.UpdateLastLogin(r.Context(), u.ID, time.Now().UTC()); err != nil && !errors.Is(err, user.ErrNotFound) {
		// Non-fatal but observable.
		h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
			"result": "success_mfa",
			"warn":   "update_last_login_failed",
		})
	}

	if usedRecovery {
		h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionMFARecoveryUse, r, nil)
	} else {
		h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionMFAVerify, r, map[string]any{
			"result": "success",
		})
	}
	h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionLogin, r, map[string]any{
		"result": "success_mfa",
		"factor": factorLabel(usedRecovery),
	})

	writeToken(w, h.h.jwtSecret, u.ID, u.WorkspaceID, u.Role)
}

func factorLabel(usedRecovery bool) string {
	if usedRecovery {
		return "recovery_code"
	}
	return "totp"
}

// Disable removes the user's TOTP credential and all recovery
// codes. Guarded by a password re-verify so a stolen session token
// cannot quietly downgrade the user's auth posture.
func (h *TOTPHandler) Disable(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	var req totpDisableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.Password == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "password is required")
		return
	}

	u, err := h.h.users.GetByID(r.Context(), claims.WorkspaceID, claims.UserID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "lookup user: "+err.Error())
		return
	}
	if err := h.h.users.VerifyPassword(u, req.Password); err != nil {
		// Audit the failed disable so a session-token-only
		// attacker probing the password is visible to operators.
		h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionMFADisable, r, map[string]any{
			"result": "password_mismatch",
		})
		// Distinct from AUTH_INVALID_TOKEN: the user IS authenticated
		// (session JWT is valid; that's how they reached this handler),
		// they just typed the wrong password on the step-up reverify.
		// Using AUTH_INVALID_TOKEN here would trigger the frontend's
		// session-clear-and-redirect interceptor and log them out
		// entirely — discarding a valid session for a recoverable
		// form-level error. AUTH_PASSWORD_REVERIFY_FAILED is in
		// NON_SESSION_401_CODES so the page's catch block handles it.
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthPasswordReverify, "invalid credentials")
		return
	}

	if err := h.h.totp.Disable(r.Context(), claims.UserID); err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "disable: "+err.Error())
		return
	}

	h.h.logAudit(r.Context(), u.WorkspaceID, &u.ID, audit.ActionMFADisable, r, map[string]any{
		"result": "success",
	})

	w.WriteHeader(http.StatusNoContent)
}

// Status returns the user's enrollment state. Frontend uses this to
// drive the account-settings badge (Enabled / Pending / Disabled)
// and the low-recovery-code warning (Remaining <= 2).
func (h *TOTPHandler) Status(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	st, err := h.h.totp.Status(r.Context(), claims.UserID)
	if err != nil {
		middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "status: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, totpStatusResponse{
		Enabled:                st.Enabled,
		PendingEnrollment:      st.PendingEnrollment,
		ActivatedAt:            st.ActivatedAt,
		LastUsedAt:             st.LastUsedAt,
		RecoveryCodesRemaining: st.RecoveryCodesRemaining,
	})
}

