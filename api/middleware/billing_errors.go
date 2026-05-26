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
		// Use the sentinel's own string rather than err.Error()
		// so we are future-proof against callers that wrap with
		// extra context (e.g. fmt.Errorf("storage: %w",
		// billing.ErrQuotaExceeded)). errors.Is still matches a
		// wrapped sentinel, but err.Error() on the wrap leaks
		// the wrapping context into the response body. Devin
		// Review ANALYSIS_0002 on commit 8d0f38e flagged this
		// as the one place a fmt.Errorf wrap could exfiltrate
		// path/driver/SQL hints through what is otherwise the
		// safe, locale-translated 402 message.
		RespondError(w, http.StatusPaymentRequired, ErrCodeQuotaExceeded, billing.ErrQuotaExceeded.Error())
		return true
	}
	return false
}
