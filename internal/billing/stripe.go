package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/client"
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

// stripePlanReader is the optional read-side dependency used by the
// admin checkout/portal flows to look up an existing customer ID.
// It is intentionally optional: the webhook path doesn't need it,
// and the unit tests can pass nil to keep their fake repos minimal.
type stripePlanReader interface {
	GetPlan(ctx context.Context, workspaceID uuid.UUID) (*Plan, error)
}

// stripeAPI abstracts the subset of the Stripe SDK used for
// server-initiated calls (Checkout / Billing Portal). The default
// implementation calls the real SDK; tests substitute a fake.
type stripeAPI interface {
	NewCheckoutSession(params *stripe.CheckoutSessionParams) (*stripe.CheckoutSession, error)
	NewPortalSession(params *stripe.BillingPortalSessionParams) (*stripe.BillingPortalSession, error)
}

// liveStripeAPI is the production stripeAPI that talks to Stripe
// over HTTPS. The secret key is bound to its own *client.API so
// concurrent services with different keys don't trample the global
// stripe.Key var.
type liveStripeAPI struct {
	sc *client.API
}

func (l *liveStripeAPI) NewCheckoutSession(p *stripe.CheckoutSessionParams) (*stripe.CheckoutSession, error) {
	if l == nil || l.sc == nil {
		// Fall back to the package-level helper which uses the
		// global stripe.Key. Useful in tests that don't go through
		// NewStripeService.
		return checkoutsession.New(p)
	}
	return l.sc.CheckoutSessions.New(p)
}

func (l *liveStripeAPI) NewPortalSession(p *stripe.BillingPortalSessionParams) (*stripe.BillingPortalSession, error) {
	if l == nil || l.sc == nil {
		return session.New(p)
	}
	return l.sc.BillingPortalSessions.New(p)
}

// StripeService wires the Stripe SDK and webhook signature checks
// to the billing.Service. It serves both inbound webhook deliveries
// (POST /api/webhooks/stripe) and admin-initiated Checkout / Customer
// Portal session creation.
//
// Construction is intentionally tolerant: the webhook handler is
// safe to mount even when secrets are empty (every request fails
// closed with a 400) and the admin endpoints return a typed error
// when the SDK is unconfigured so the HTTP layer can map them to
// 501 Not Implemented.
type StripeService struct {
	svc          stripeUpserter
	reader       stripePlanReader
	secret       string
	priceTierMap map[string]string
	tierPriceMap map[string]string
	api          stripeAPI
	hasSecretKey bool
}

// StripeWebhookHandler is the legacy alias for StripeService. Kept
// to minimise churn in older call sites; new code should use
// StripeService directly.
type StripeWebhookHandler = StripeService

// ErrStripeNotConfigured is returned by the admin checkout / portal
// methods when the Stripe SDK is unconfigured (no secret key) or
// when a portal session is requested for a workspace that has no
// stripe_customer_id on file. Handlers map this to 501 / 412.
var ErrStripeNotConfigured = errors.New("billing/stripe: not configured")

// ErrStripePriceNotMapped is returned by CreateCheckoutSession when
// the requested tier has no Stripe price ID configured. Handlers
// map this to 400 Bad Request so the operator knows to fix the
// STRIPE_PRICE_TIER_MAP env var.
var ErrStripePriceNotMapped = errors.New("billing/stripe: tier has no stripe price configured")

// ErrStripeNoCustomer is returned by CreatePortalSession when the
// workspace has no stripe_customer_id yet (i.e. the admin hasn't
// completed a Checkout flow). Handlers map this to 412 Precondition
// Failed so the frontend can surface a clear message.
var ErrStripeNoCustomer = errors.New("billing/stripe: workspace has no stripe customer on file")

// NewStripeService wires up a service against the supplied billing
// service, repository (used to look up stripe_customer_id), Stripe
// secrets, and price-tier map. A nil reader, empty secretKey, or
// empty priceTierMap each disable the corresponding feature in a
// well-defined way (see ErrStripeNotConfigured / ErrStripeNoCustomer
// / ErrStripePriceNotMapped).
func NewStripeService(svc *Service, reader stripePlanReader, webhookSecret, secretKey string, priceTierMap map[string]string) *StripeService {
	return newStripeService(svc, reader, webhookSecret, secretKey, priceTierMap, nil)
}

// NewStripeWebhookHandler is the legacy constructor signature kept
// for compatibility with older callers. It is equivalent to
// NewStripeService with no plan reader and no secret key — i.e.
// webhook-only wiring. New code should call NewStripeService.
func NewStripeWebhookHandler(svc *Service, secret string, priceTierMap map[string]string) *StripeService {
	return newStripeService(svc, nil, secret, "", priceTierMap, nil)
}

func newStripeService(
	svc *Service,
	reader stripePlanReader,
	webhookSecret, secretKey string,
	priceTierMap map[string]string,
	api stripeAPI,
) *StripeService {
	s := &StripeService{
		svc:          svc,
		reader:       reader,
		secret:       webhookSecret,
		priceTierMap: priceTierMap,
		tierPriceMap: invertPriceTierMap(priceTierMap),
		hasSecretKey: secretKey != "",
	}
	if api != nil {
		s.api = api
	} else if secretKey != "" {
		s.api = &liveStripeAPI{sc: client.New(secretKey, nil)}
	} else {
		// No API key: keep a nil-tolerant adapter so the
		// admin-initiated paths return ErrStripeNotConfigured
		// rather than panicking on a nil deref.
		s.api = &liveStripeAPI{}
	}
	return s
}

func invertPriceTierMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	// When multiple price IDs map to the same tier (e.g. a monthly
	// and an annual price for the "business" tier), the last one
	// wins. Operators who care about a specific cadence should set
	// the canonical price last in the env-var ordering — or use a
	// dedicated tier name per cadence.
	for priceID, tier := range in {
		if tier == "" {
			continue
		}
		out[tier] = priceID
	}
	return out
}

// HandleWebhook is the chi-compatible HTTP handler for
// POST /api/webhooks/stripe.
func (h *StripeService) HandleWebhook(w http.ResponseWriter, r *http.Request) {
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

func (h *StripeService) dispatch(ctx context.Context, event *stripe.Event) error {
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

func (h *StripeService) handleCheckoutCompleted(ctx context.Context, event *stripe.Event) error {
	var checkoutSession stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &checkoutSession); err != nil {
		return err
	}
	workspaceID, err := workspaceIDFromMetadata(checkoutSession.Metadata)
	if err != nil {
		return err
	}
	tier := tierFromMetadata(checkoutSession.Metadata)
	if tier == "" {
		// Checkout sessions in the wild may also expand the
		// underlying subscription; fall back to its line items
		// when the session metadata didn't carry the tier.
		if checkoutSession.Subscription != nil {
			tier = h.tierFromSubscription(checkoutSession.Subscription)
		}
	}
	if !IsValidTier(tier) {
		return errors.New("billing/stripe: checkout.session.completed missing valid tier metadata")
	}
	customerID := customerIDFromCheckoutSession(&checkoutSession)
	return h.upsert(ctx, workspaceID, tier, customerID)
}

func (h *StripeService) handleSubscriptionUpdated(ctx context.Context, event *stripe.Event) error {
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
	return h.upsert(ctx, workspaceID, tier, customerIDFromSubscription(&sub))
}

func (h *StripeService) handleSubscriptionDeleted(ctx context.Context, event *stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return err
	}
	workspaceID, err := workspaceIDFromMetadata(sub.Metadata)
	if err != nil {
		return err
	}
	return h.upsert(ctx, workspaceID, TierFree, customerIDFromSubscription(&sub))
}

// tierFromSubscription resolves a tier from the first subscription
// item's price. Most Stripe subscriptions carry a single recurring
// item; multi-item subscriptions are uncommon for SaaS plans and
// taking the first item matches Stripe's own dashboard semantics.
func (h *StripeService) tierFromSubscription(sub *stripe.Subscription) string {
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

func (h *StripeService) upsert(ctx context.Context, workspaceID uuid.UUID, tier string, customerID string) error {
	plan := &Plan{
		WorkspaceID: workspaceID,
		Tier:        tier,
	}
	if customerID != "" {
		plan.StripeCustomerID = &customerID
	}
	_, err := h.svc.UpsertPlan(ctx, plan)
	return err
}

// CreateCheckoutSession provisions a Stripe Checkout session for
// the supplied workspace + tier and returns the redirect URL the
// admin should be sent to. The session carries `metadata.workspace_id`
// and `metadata.tier` so the webhook handler can resolve the plan
// even when Stripe doesn't expand the subscription on the
// completion event.
func (h *StripeService) CreateCheckoutSession(
	ctx context.Context,
	workspaceID uuid.UUID,
	tier, successURL, cancelURL string,
) (string, error) {
	if h == nil || !h.hasSecretKey || h.api == nil {
		return "", ErrStripeNotConfigured
	}
	if !IsValidTier(tier) {
		return "", errors.New("billing/stripe: invalid tier")
	}
	priceID, ok := h.tierPriceMap[tier]
	if !ok || priceID == "" {
		return "", fmt.Errorf("%w: %s", ErrStripePriceNotMapped, tier)
	}
	if successURL == "" || cancelURL == "" {
		return "", errors.New("billing/stripe: success_url and cancel_url are required")
	}

	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
	}
	// Reuse the existing Stripe customer when the workspace already
	// has one on file; otherwise let Stripe create a new customer
	// during the Checkout flow. The webhook handler will persist
	// the new ID on `checkout.session.completed`.
	if existing := h.lookupCustomerID(ctx, workspaceID); existing != "" {
		params.Customer = stripe.String(existing)
	}
	params.AddMetadata("workspace_id", workspaceID.String())
	params.AddMetadata("tier", tier)
	// Mirror the metadata onto the resulting subscription so
	// later customer.subscription.updated / .deleted events carry
	// it without an extra round-trip to fetch the parent session.
	params.SubscriptionData = &stripe.CheckoutSessionSubscriptionDataParams{}
	params.SubscriptionData.AddMetadata("workspace_id", workspaceID.String())
	params.SubscriptionData.AddMetadata("tier", tier)

	cs, err := h.api.NewCheckoutSession(params)
	if err != nil {
		return "", fmt.Errorf("billing/stripe: create checkout session: %w", err)
	}
	if cs == nil || cs.URL == "" {
		return "", errors.New("billing/stripe: stripe returned an empty checkout url")
	}
	return cs.URL, nil
}

// CreatePortalSession provisions a Customer Portal session for the
// workspace's existing Stripe customer. Returns ErrStripeNoCustomer
// when the workspace has not yet completed a Checkout (so the
// frontend can prompt the admin to subscribe first).
func (h *StripeService) CreatePortalSession(
	ctx context.Context,
	stripeCustomerID, returnURL string,
) (string, error) {
	if h == nil || !h.hasSecretKey || h.api == nil {
		return "", ErrStripeNotConfigured
	}
	if stripeCustomerID == "" {
		return "", ErrStripeNoCustomer
	}
	if returnURL == "" {
		return "", errors.New("billing/stripe: return_url is required")
	}
	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(stripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	}
	ps, err := h.api.NewPortalSession(params)
	if err != nil {
		return "", fmt.Errorf("billing/stripe: create portal session: %w", err)
	}
	if ps == nil || ps.URL == "" {
		return "", errors.New("billing/stripe: stripe returned an empty portal url")
	}
	return ps.URL, nil
}

// LookupCustomerID exposes the plan-row stripe_customer_id lookup
// used by the admin portal-session handler. Returns the empty
// string when no plan row exists or the field is unset.
func (h *StripeService) LookupCustomerID(ctx context.Context, workspaceID uuid.UUID) string {
	return h.lookupCustomerID(ctx, workspaceID)
}

func (h *StripeService) lookupCustomerID(ctx context.Context, workspaceID uuid.UUID) string {
	if h == nil || h.reader == nil {
		return ""
	}
	plan, err := h.reader.GetPlan(ctx, workspaceID)
	if err != nil || plan == nil || plan.StripeCustomerID == nil {
		return ""
	}
	return *plan.StripeCustomerID
}

func customerIDFromCheckoutSession(s *stripe.CheckoutSession) string {
	if s == nil {
		return ""
	}
	if s.Customer != nil && s.Customer.ID != "" {
		return s.Customer.ID
	}
	return ""
}

func customerIDFromSubscription(sub *stripe.Subscription) string {
	if sub == nil {
		return ""
	}
	if sub.Customer != nil && sub.Customer.ID != "" {
		return sub.Customer.ID
	}
	return ""
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
