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
		http.Error(w, "stripe billing not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	actor, _ := middleware.UserIDFromContext(r.Context())

	var req checkoutSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if !billing.IsValidTier(req.Tier) {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}
	if req.SuccessURL == "" || req.CancelURL == "" {
		http.Error(w, "success_url and cancel_url are required", http.StatusBadRequest)
		return
	}

	url, err := h.stripe.CreateCheckoutSession(r.Context(), workspaceID, req.Tier, req.SuccessURL, req.CancelURL)
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrStripeNotConfigured):
			http.Error(w, "stripe not configured", http.StatusNotImplemented)
		case errors.Is(err, billing.ErrStripePriceNotMapped):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, "create checkout session: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "stripe billing not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	actor, _ := middleware.UserIDFromContext(r.Context())

	var req portalSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.ReturnURL == "" {
		http.Error(w, "return_url is required", http.StatusBadRequest)
		return
	}

	customerID := h.stripe.LookupCustomerID(r.Context(), workspaceID)
	if customerID == "" {
		http.Error(w, "workspace has no stripe customer on file", http.StatusPreconditionFailed)
		return
	}

	url, err := h.stripe.CreatePortalSession(r.Context(), customerID, req.ReturnURL)
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrStripeNotConfigured):
			http.Error(w, "stripe not configured", http.StatusNotImplemented)
		case errors.Is(err, billing.ErrStripeNoCustomer):
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
		default:
			http.Error(w, "create portal session: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if h.audit != nil {
		h.audit.LogAction(r.Context(), workspaceID, &actor, audit.ActionAdminBillingPortal, "billing_plan", nil, r, nil)
	}
	writeJSON(w, http.StatusOK, sessionURLResponse{URL: url})
}
