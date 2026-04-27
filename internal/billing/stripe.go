package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"
)

// stripeMaxBodyBytes caps the webhook payload at 64 KiB. Stripe
// webhooks are typically a few kilobytes; the limit defends against
// a misbehaving (or malicious) sender filling memory before the
// signature can be verified.
const stripeMaxBodyBytes = int64(64 * 1024)

// stripeUpserter is the subset of *Service the webhook handler
// needs. Splitting it out keeps the tests honest — they swap in a
// fake without spinning up Postgres.
type stripeUpserter interface {
	UpsertPlan(ctx context.Context, p *Plan) (*Plan, error)
}

// StripeWebhookHandler turns Stripe subscription lifecycle events
// into UpsertPlan calls against the billing service. It is mounted
// outside the auth middleware group because Stripe authenticates
// itself via the Stripe-Signature header rather than a JWT.
type StripeWebhookHandler struct {
	svc          stripeUpserter
	secret       string
	priceTierMap map[string]string
}

// NewStripeWebhookHandler wires up a handler against the supplied
// billing service. priceTierMap is consulted before falling back to
// price metadata when resolving a tier from a subscription event.
// It may be nil.
func NewStripeWebhookHandler(svc *Service, secret string, priceTierMap map[string]string) *StripeWebhookHandler {
	return &StripeWebhookHandler{
		svc:          svc,
		secret:       secret,
		priceTierMap: priceTierMap,
	}
}

// HandleWebhook is the chi-compatible HTTP handler for
// POST /api/webhooks/stripe.
func (h *StripeWebhookHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil || h.secret == "" {
		http.Error(w, "stripe webhook not configured", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, stripeMaxBodyBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusBadRequest)
		return
	}

	event, err := webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), h.secret)
	if err != nil {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	if err := h.dispatch(r.Context(), &event); err != nil {
		log.Printf("billing/stripe: dispatch %s (%s) failed: %v", event.Type, event.ID, err)
		http.Error(w, "processing failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (h *StripeWebhookHandler) dispatch(ctx context.Context, event *stripe.Event) error {
	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted:
		return h.handleCheckoutCompleted(ctx, event)
	case stripe.EventTypeCustomerSubscriptionUpdated:
		return h.handleSubscriptionUpdated(ctx, event)
	case stripe.EventTypeCustomerSubscriptionDeleted:
		return h.handleSubscriptionDeleted(ctx, event)
	}
	// Unhandled event types are still a successful delivery — the
	// caller writes 200 so Stripe doesn't retry forever. Logging at
	// debug level would be ideal; for now we stay quiet to avoid
	// log spam from Stripe's many event categories.
	return nil
}

func (h *StripeWebhookHandler) handleCheckoutCompleted(ctx context.Context, event *stripe.Event) error {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return err
	}
	workspaceID, err := workspaceIDFromMetadata(session.Metadata)
	if err != nil {
		return err
	}
	tier := tierFromMetadata(session.Metadata)
	if tier == "" {
		// Checkout sessions in the wild may also expand the
		// underlying subscription; fall back to its line items
		// when the session metadata didn't carry the tier.
		if session.Subscription != nil {
			tier = h.tierFromSubscription(session.Subscription)
		}
	}
	if !IsValidTier(tier) {
		return errors.New("billing/stripe: checkout.session.completed missing valid tier metadata")
	}
	return h.upsert(ctx, workspaceID, tier)
}

func (h *StripeWebhookHandler) handleSubscriptionUpdated(ctx context.Context, event *stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return err
	}
	workspaceID, err := workspaceIDFromMetadata(sub.Metadata)
	if err != nil {
		return err
	}
	tier := h.tierFromSubscription(&sub)
	if !IsValidTier(tier) {
		return errors.New("billing/stripe: customer.subscription.updated could not resolve tier")
	}
	return h.upsert(ctx, workspaceID, tier)
}

func (h *StripeWebhookHandler) handleSubscriptionDeleted(ctx context.Context, event *stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return err
	}
	workspaceID, err := workspaceIDFromMetadata(sub.Metadata)
	if err != nil {
		return err
	}
	return h.upsert(ctx, workspaceID, TierFree)
}

// tierFromSubscription resolves a tier from the first subscription
// item's price. Most Stripe subscriptions carry a single recurring
// item; multi-item subscriptions are uncommon for SaaS plans and
// taking the first item matches Stripe's own dashboard semantics.
func (h *StripeWebhookHandler) tierFromSubscription(sub *stripe.Subscription) string {
	if sub == nil || sub.Items == nil {
		return ""
	}
	for _, item := range sub.Items.Data {
		if item == nil || item.Price == nil {
			continue
		}
		if t, ok := h.priceTierMap[item.Price.ID]; ok && IsValidTier(t) {
			return t
		}
		if t := tierFromMetadata(item.Price.Metadata); t != "" {
			return t
		}
	}
	return ""
}

func (h *StripeWebhookHandler) upsert(ctx context.Context, workspaceID uuid.UUID, tier string) error {
	_, err := h.svc.UpsertPlan(ctx, &Plan{
		WorkspaceID: workspaceID,
		Tier:        tier,
	})
	return err
}

func workspaceIDFromMetadata(md map[string]string) (uuid.UUID, error) {
	raw, ok := md["workspace_id"]
	if !ok || raw == "" {
		return uuid.Nil, errors.New("billing/stripe: missing workspace_id metadata")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, errors.New("billing/stripe: invalid workspace_id metadata")
	}
	return id, nil
}

func tierFromMetadata(md map[string]string) string {
	if md == nil {
		return ""
	}
	tier := md["tier"]
	if !IsValidTier(tier) {
		return ""
	}
	return tier
}
