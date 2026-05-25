package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
)

// WithStripe wires the Stripe service so the admin checkout-session
// and portal-session endpoints stop responding 501 Not Implemented.
// A nil service keeps them disabled.
func (h *Handler) WithStripe(s *billing.StripeService) *Handler {
	h.stripe = s
	return h
}

// checkoutSessionRequest is the JSON body accepted by
// POST /api/admin/billing/checkout-session. The success / cancel
// URLs are caller-supplied so the frontend can route the user back
// to the correct page after Stripe Checkout returns.
type checkoutSessionRequest struct {
	Tier       string `json:"tier"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

// portalSessionRequest is the JSON body accepted by
// POST /api/admin/billing/portal-session.
type portalSessionRequest struct {
	ReturnURL string `json:"return_url"`
}

// sessionURLResponse is the shared response shape for both endpoints.
// Returning a single `url` field keeps the frontend symmetric: it
// just window.location.assigns the value either way.
type sessionURLResponse struct {
	URL string `json:"url"`
}

// CreateCheckoutSession returns a Stripe Checkout URL for the
// admin's workspace + the requested tier. The frontend redirects the
// browser to the returned URL; on success the webhook handler
// upserts the plan row.
func (h *Handler) CreateCheckoutSession(w http.ResponseWriter, r *http.Request) {
	if h.stripe == nil || h.billing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "stripe billing not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	actor, _ := middleware.UserIDFromContext(r.Context())

	var req checkoutSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if !billing.IsValidTier(req.Tier) {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid tier")
		return
	}
	if req.SuccessURL == "" || req.CancelURL == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "success_url and cancel_url are required")
		return
	}

	url, err := h.stripe.CreateCheckoutSession(r.Context(), workspaceID, req.Tier, req.SuccessURL, req.CancelURL)
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrStripeNotConfigured):
			middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "stripe not configured")
		case errors.Is(err, billing.ErrStripePriceNotMapped):
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
		default:
			middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "create checkout session: "+err.Error())
		}
		return
	}

	if h.audit != nil {
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminBillingCheckout, "billing_plan", nil, r, map[string]any{
			"tier": req.Tier,
		})
	}
	writeJSON(w, http.StatusOK, sessionURLResponse{URL: url})
}

// CreatePortalSession returns a Stripe Customer Portal URL for the
// workspace's existing customer. The frontend redirects the browser
// to the returned URL; on return the user lands at return_url.
func (h *Handler) CreatePortalSession(w http.ResponseWriter, r *http.Request) {
	if h.stripe == nil || h.billing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "stripe billing not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	actor, _ := middleware.UserIDFromContext(r.Context())

	var req portalSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.ReturnURL == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "return_url is required")
		return
	}

	customerID := h.stripe.LookupCustomerID(r.Context(), workspaceID)
	if customerID == "" {
		middleware.RespondError(w, http.StatusPreconditionFailed, middleware.ErrCodeInternal, "workspace has no stripe customer on file")
		return
	}

	url, err := h.stripe.CreatePortalSession(r.Context(), customerID, req.ReturnURL)
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrStripeNotConfigured):
			middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "stripe not configured")
		case errors.Is(err, billing.ErrStripeNoCustomer):
			middleware.RespondError(w, http.StatusPreconditionFailed, middleware.ErrCodeInternal, err.Error())
		default:
			middleware.RespondError(w, http.StatusInternalServerError, middleware.ErrCodeInternal, "create portal session: "+err.Error())
		}
		return
	}

	if h.audit != nil {
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminBillingPortal, "billing_plan", nil, r, nil)
	}
	writeJSON(w, http.StatusOK, sessionURLResponse{URL: url})
}
