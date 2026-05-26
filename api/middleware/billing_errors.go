package middleware

import (
	"errors"
	"net/http"

	"github.com/kennguy3n/zk-drive/internal/billing"
)

// WriteBillingError handles the canonical billing-error -> HTTP
// mapping in one place so every handler that touches a billing call
// returns the same code for the same condition. Today the only
// mapped sentinel is billing.ErrQuotaExceeded -> 402 Payment Required
// with code WORKSPACE_QUOTA_EXCEEDED, but new sentinels (trial
// expired, payment method missing, downgrade-blocked) should be added
// here exactly once.
//
// Returns true if it recognised and wrote err, false if the caller
// must fall through to its package-local error helper. Pattern:
//
//	if middleware.WriteBillingError(w, err) {
//	    return
//	}
//	writeServiceError(w, err)
//
// The intentional split between this helper (billing-specific mapping)
// and the caller's fallback (writeServiceError vs RespondError with
// ErrCodeInternal) avoids importing every caller's service-error
// taxonomy into the middleware package, which would either create
// circular dependencies (api/drive -> api/middleware -> api/drive) or
// force every fallback to be inlined here. The two writeBillingError
// helpers that previously lived in api/drive/upload.go and
// api/admin/handler.go drifted to use different fallbacks (drive used
// writeServiceError, admin used RespondError(InternalError) directly)
// and would have drifted further as new billing sentinels were added.
func WriteBillingError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, billing.ErrQuotaExceeded):
		RespondError(w, http.StatusPaymentRequired, ErrCodeQuotaExceeded, err.Error())
		return true
	}
	return false
}
