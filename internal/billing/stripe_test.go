package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v81"
)

// fakeRepo implements just enough of Repository for the webhook
// tests: UpsertPlan stores the supplied row and returns it; every
// other method panics so an accidental call surfaces immediately.
type fakeRepo struct {
	mu    sync.Mutex
	calls []*Plan
}

func (f *fakeRepo) UpsertPlan(_ context.Context, p *Plan) (*Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *p
	if clone.ID == uuid.Nil {
		clone.ID = uuid.New()
	}
	clone.CreatedAt = time.Now()
	clone.UpdatedAt = clone.CreatedAt
	f.calls = append(f.calls, &clone)
	return &clone, nil
}

func (f *fakeRepo) lastUpsert() *Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

// The remaining Repository methods are unused by the webhook flow.
func (f *fakeRepo) GetPlan(context.Context, uuid.UUID) (*Plan, error) {
	panic("unexpected GetPlan in stripe webhook test")
}
func (f *fakeRepo) RecordEvent(context.Context, uuid.UUID, string, int64) error {
	panic("unexpected RecordEvent in stripe webhook test")
}
func (f *fakeRepo) GetStorageUsed(context.Context, uuid.UUID) (int64, error) {
	panic("unexpected GetStorageUsed in stripe webhook test")
}
func (f *fakeRepo) GetBandwidthUsedThisMonth(context.Context, uuid.UUID) (int64, error) {
	panic("unexpected GetBandwidthUsedThisMonth in stripe webhook test")
}
func (f *fakeRepo) GetUserCount(context.Context, uuid.UUID) (int, error) {
	panic("unexpected GetUserCount in stripe webhook test")
}

// signedRequest builds a POST request to /api/webhooks/stripe with a
// valid Stripe-Signature header for the supplied payload.
func signedRequest(t *testing.T, payload []byte, secret string) *http.Request {
	t.Helper()
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d", ts)
	mac.Write([]byte("."))
	mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%d,v1=%s", ts, sig)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/stripe", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", header)
	return req
}

// buildEventPayload wraps a raw object in a stripe.Event envelope
// signed for the current API version so ConstructEvent doesn't reject
// the payload as version-mismatched.
func buildEventPayload(t *testing.T, eventType stripe.EventType, object map[string]interface{}) []byte {
	t.Helper()
	rawObject, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}
	envelope := map[string]interface{}{
		"id":          "evt_test_" + uuid.NewString(),
		"object":      "event",
		"api_version": stripe.APIVersion,
		"created":     time.Now().Unix(),
		"type":        string(eventType),
		"data": map[string]interface{}{
			"object": json.RawMessage(rawObject),
		},
		"livemode":         false,
		"pending_webhooks": 0,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func TestStripeWebhookProvisionsPlan(t *testing.T) {
	const secret = "whsec_test_secret"
	repo := &fakeRepo{}
	svc := NewService(repo)
	handler := NewStripeWebhookHandler(svc, secret, nil)

	workspaceID := uuid.New()
	payload := buildEventPayload(t, stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"id":     "cs_test_" + uuid.NewString(),
		"object": "checkout.session",
		"metadata": map[string]string{
			"workspace_id": workspaceID.String(),
			"tier":         TierBusiness,
		},
		"mode":   "subscription",
		"status": "complete",
	})

	rec := httptest.NewRecorder()
	handler.HandleWebhook(rec, signedRequest(t, payload, secret))

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Result().Body)
		t.Fatalf("expected 200, got %d: %s", rec.Code, strings.TrimSpace(string(body)))
	}

	got := repo.lastUpsert()
	if got == nil {
		t.Fatalf("expected UpsertPlan to be called, got no calls")
	}
	if got.WorkspaceID != workspaceID {
		t.Errorf("workspace_id: got %s, want %s", got.WorkspaceID, workspaceID)
	}
	if got.Tier != TierBusiness {
		t.Errorf("tier: got %q, want %q", got.Tier, TierBusiness)
	}
}

func TestStripeSignatureRejection(t *testing.T) {
	const secret = "whsec_test_secret"
	repo := &fakeRepo{}
	handler := NewStripeWebhookHandler(NewService(repo), secret, nil)

	cases := []struct {
		name      string
		signature string
	}{
		{name: "missing", signature: ""},
		{name: "garbage", signature: "t=1,v1=deadbeef"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"id":"evt_test","type":"checkout.session.completed"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/webhooks/stripe", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tc.signature != "" {
				req.Header.Set("Stripe-Signature", tc.signature)
			}

			rec := httptest.NewRecorder()
			handler.HandleWebhook(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", rec.Code)
			}
			if calls := len(repo.calls); calls != 0 {
				t.Fatalf("UpsertPlan must not be called on signature failure, got %d calls", calls)
			}
		})
	}
}

// TestStripeWebhookCapturesCustomerID asserts that a
// checkout.session.completed event with a `customer` field flows
// through to UpsertPlan with stripe_customer_id populated, so the
// later portal-session lookup can find it.
func TestStripeWebhookCapturesCustomerID(t *testing.T) {
	const secret = "whsec_test_secret"
	repo := &fakeRepo{}
	handler := NewStripeWebhookHandler(NewService(repo), secret, nil)

	workspaceID := uuid.New()
	const customerID = "cus_test_capturetest"
	payload := buildEventPayload(t, stripe.EventTypeCheckoutSessionCompleted, map[string]interface{}{
		"id":     "cs_test_" + uuid.NewString(),
		"object": "checkout.session",
		"metadata": map[string]string{
			"workspace_id": workspaceID.String(),
			"tier":         TierStarter,
		},
		"customer": map[string]interface{}{
			"id":     customerID,
			"object": "customer",
		},
		"mode":   "subscription",
		"status": "complete",
	})

	rec := httptest.NewRecorder()
	handler.HandleWebhook(rec, signedRequest(t, payload, secret))

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Result().Body)
		t.Fatalf("expected 200, got %d: %s", rec.Code, strings.TrimSpace(string(body)))
	}

	got := repo.lastUpsert()
	if got == nil {
		t.Fatalf("expected UpsertPlan to be called")
	}
	if got.StripeCustomerID == nil {
		t.Fatalf("expected stripe_customer_id to be captured, got nil")
	}
	if *got.StripeCustomerID != customerID {
		t.Errorf("stripe_customer_id: got %q, want %q", *got.StripeCustomerID, customerID)
	}
}

// fakeStripeAPI implements stripeAPI without talking to Stripe. It
// records the params passed in and returns canned URLs so the unit
// tests can assert what the service builds.
type fakeStripeAPI struct {
	checkoutParams *stripe.CheckoutSessionParams
	portalParams   *stripe.BillingPortalSessionParams
	checkoutURL    string
	portalURL      string
	checkoutErr    error
	portalErr      error
}

func (f *fakeStripeAPI) NewCheckoutSession(p *stripe.CheckoutSessionParams) (*stripe.CheckoutSession, error) {
	f.checkoutParams = p
	if f.checkoutErr != nil {
		return nil, f.checkoutErr
	}
	return &stripe.CheckoutSession{URL: f.checkoutURL}, nil
}

func (f *fakeStripeAPI) NewPortalSession(p *stripe.BillingPortalSessionParams) (*stripe.BillingPortalSession, error) {
	f.portalParams = p
	if f.portalErr != nil {
		return nil, f.portalErr
	}
	return &stripe.BillingPortalSession{URL: f.portalURL}, nil
}

// fakePlanReader implements stripePlanReader for the
// CreatePortalSession lookup. It serves a single plan keyed by
// workspace ID.
type fakePlanReader struct {
	plans map[uuid.UUID]*Plan
}

func (f *fakePlanReader) GetPlan(_ context.Context, workspaceID uuid.UUID) (*Plan, error) {
	if p, ok := f.plans[workspaceID]; ok {
		return p, nil
	}
	return nil, ErrPlanNotFound
}

func TestCreateCheckoutSessionAttachesMetadata(t *testing.T) {
	repo := &fakeRepo{}
	api := &fakeStripeAPI{checkoutURL: "https://checkout.stripe.com/c/pay/cs_test_123"}
	svc := newStripeService(
		NewService(repo),
		nil,
		"whsec_test",
		"sk_test_secret",
		map[string]string{"price_business_monthly": TierBusiness},
		api,
	)

	workspaceID := uuid.New()
	url, err := svc.CreateCheckoutSession(
		context.Background(),
		workspaceID,
		TierBusiness,
		"https://app.example.com/billing?stripe=success",
		"https://app.example.com/billing?stripe=cancel",
	)
	if err != nil {
		t.Fatalf("create checkout: %v", err)
	}
	if url != api.checkoutURL {
		t.Errorf("returned url: got %q, want %q", url, api.checkoutURL)
	}
	if api.checkoutParams == nil {
		t.Fatalf("expected stripe params to be captured")
	}
	if got := api.checkoutParams.Metadata["workspace_id"]; got != workspaceID.String() {
		t.Errorf("metadata.workspace_id: got %q, want %q", got, workspaceID.String())
	}
	if got := api.checkoutParams.Metadata["tier"]; got != TierBusiness {
		t.Errorf("metadata.tier: got %q, want %q", got, TierBusiness)
	}
	if api.checkoutParams.Mode == nil || *api.checkoutParams.Mode != "subscription" {
		t.Errorf("mode: expected subscription, got %v", api.checkoutParams.Mode)
	}
	if len(api.checkoutParams.LineItems) != 1 || api.checkoutParams.LineItems[0].Price == nil ||
		*api.checkoutParams.LineItems[0].Price != "price_business_monthly" {
		t.Errorf("line_items: expected single price_business_monthly entry, got %+v", api.checkoutParams.LineItems)
	}
}

func TestCreateCheckoutSessionRejectsUnmappedTier(t *testing.T) {
	api := &fakeStripeAPI{}
	svc := newStripeService(
		NewService(&fakeRepo{}),
		nil,
		"whsec_test",
		"sk_test_secret",
		map[string]string{"price_business_monthly": TierBusiness},
		api,
	)

	_, err := svc.CreateCheckoutSession(
		context.Background(),
		uuid.New(),
		TierStarter,
		"https://app.example.com/ok",
		"https://app.example.com/cancel",
	)
	if !errors.Is(err, ErrStripePriceNotMapped) {
		t.Fatalf("expected ErrStripePriceNotMapped, got %v", err)
	}
	if api.checkoutParams != nil {
		t.Errorf("Stripe SDK should not be invoked when the tier has no price mapping")
	}
}

func TestCreatePortalSessionRequiresCustomer(t *testing.T) {
	api := &fakeStripeAPI{portalURL: "https://billing.stripe.com/session/test_portal"}
	svc := newStripeService(
		NewService(&fakeRepo{}),
		&fakePlanReader{plans: map[uuid.UUID]*Plan{}},
		"",
		"sk_test_secret",
		nil,
		api,
	)

	_, err := svc.CreatePortalSession(context.Background(), "", "https://app.example.com/billing")
	if !errors.Is(err, ErrStripeNoCustomer) {
		t.Fatalf("expected ErrStripeNoCustomer, got %v", err)
	}

	url, err := svc.CreatePortalSession(context.Background(), "cus_test", "https://app.example.com/billing")
	if err != nil {
		t.Fatalf("create portal: %v", err)
	}
	if url != api.portalURL {
		t.Errorf("returned url: got %q, want %q", url, api.portalURL)
	}
	if api.portalParams == nil || api.portalParams.Customer == nil || *api.portalParams.Customer != "cus_test" {
		t.Errorf("expected customer cus_test in portal params, got %+v", api.portalParams)
	}
}

func TestStripeAdminCallsRequireSecretKey(t *testing.T) {
	svc := newStripeService(
		NewService(&fakeRepo{}),
		nil,
		"whsec_test",
		"", // no secret key configured
		map[string]string{"price_x": TierBusiness},
		&fakeStripeAPI{},
	)

	if _, err := svc.CreateCheckoutSession(
		context.Background(),
		uuid.New(),
		TierBusiness,
		"https://ok",
		"https://cancel",
	); !errors.Is(err, ErrStripeNotConfigured) {
		t.Errorf("CreateCheckoutSession without secret key: got %v, want ErrStripeNotConfigured", err)
	}
	if _, err := svc.CreatePortalSession(context.Background(), "cus_x", "https://ok"); !errors.Is(err, ErrStripeNotConfigured) {
		t.Errorf("CreatePortalSession without secret key: got %v, want ErrStripeNotConfigured", err)
	}
}
