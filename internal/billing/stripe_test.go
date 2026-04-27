package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// tests. It models the production semantics where UpdateTier
// preserves any previously-set limit overrides on the row, while
// UpsertPlan rewrites them — the regression below depends on the
// distinction.
type fakeRepo struct {
	mu        sync.Mutex
	plans     map[uuid.UUID]*Plan
	tierCalls []tierCall
}

type tierCall struct {
	workspaceID uuid.UUID
	tier        string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{plans: make(map[uuid.UUID]*Plan)}
}

func (f *fakeRepo) UpsertPlan(_ context.Context, p *Plan) (*Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *p
	if existing, ok := f.plans[p.WorkspaceID]; ok && existing.ID != uuid.Nil {
		clone.ID = existing.ID
		clone.CreatedAt = existing.CreatedAt
	} else {
		if clone.ID == uuid.Nil {
			clone.ID = uuid.New()
		}
		clone.CreatedAt = time.Now()
	}
	clone.UpdatedAt = time.Now()
	f.plans[p.WorkspaceID] = &clone
	return &clone, nil
}

func (f *fakeRepo) UpdateTier(_ context.Context, workspaceID uuid.UUID, tier string) (*Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tierCalls = append(f.tierCalls, tierCall{workspaceID: workspaceID, tier: tier})
	if existing, ok := f.plans[workspaceID]; ok {
		clone := *existing
		clone.Tier = tier
		clone.UpdatedAt = time.Now()
		f.plans[workspaceID] = &clone
		return &clone, nil
	}
	plan := &Plan{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		Tier:        tier,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	f.plans[workspaceID] = plan
	return plan, nil
}

func (f *fakeRepo) snapshot(workspaceID uuid.UUID) *Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.plans[workspaceID]; ok {
		clone := *p
		return &clone
	}
	return nil
}

func (f *fakeRepo) lastTierCall() *tierCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tierCalls) == 0 {
		return nil
	}
	tc := f.tierCalls[len(f.tierCalls)-1]
	return &tc
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
	repo := newFakeRepo()
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

	call := repo.lastTierCall()
	if call == nil {
		t.Fatalf("expected UpdateTier to be called, got no calls")
	}
	if call.workspaceID != workspaceID {
		t.Errorf("workspace_id: got %s, want %s", call.workspaceID, workspaceID)
	}
	if call.tier != TierBusiness {
		t.Errorf("tier: got %q, want %q", call.tier, TierBusiness)
	}
}

// TestStripeWebhookPreservesAdminLimitOverrides locks in the
// invariant that a routine subscription event must not clear an
// admin-configured per-workspace limit override. Stripe events only
// speak in tiers; the admin panel remains the source of truth for
// custom limit columns.
func TestStripeWebhookPreservesAdminLimitOverrides(t *testing.T) {
	const secret = "whsec_test_secret"
	repo := newFakeRepo()
	svc := NewService(repo)

	workspaceID := uuid.New()
	priceID := "price_business_pro"
	customStorage := int64(2 * 1024 * 1024 * 1024 * 1024) // 2 TB
	customUsers := 500
	if _, err := svc.UpsertPlan(context.Background(), &Plan{
		WorkspaceID:     workspaceID,
		Tier:            TierBusiness,
		MaxStorageBytes: &customStorage,
		MaxUsers:        &customUsers,
	}); err != nil {
		t.Fatalf("seed admin plan: %v", err)
	}

	handler := NewStripeWebhookHandler(svc, secret, map[string]string{priceID: TierBusiness})

	payload := buildEventPayload(t, stripe.EventTypeCustomerSubscriptionUpdated, map[string]interface{}{
		"id":     "sub_test_" + uuid.NewString(),
		"object": "subscription",
		"metadata": map[string]string{
			"workspace_id": workspaceID.String(),
		},
		"items": map[string]interface{}{
			"object": "list",
			"data": []map[string]interface{}{
				{
					"id":     "si_test_" + uuid.NewString(),
					"object": "subscription_item",
					"price": map[string]interface{}{
						"id":     priceID,
						"object": "price",
					},
				},
			},
		},
	})

	rec := httptest.NewRecorder()
	handler.HandleWebhook(rec, signedRequest(t, payload, secret))
	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Result().Body)
		t.Fatalf("expected 200, got %d: %s", rec.Code, strings.TrimSpace(string(body)))
	}

	final := repo.snapshot(workspaceID)
	if final == nil {
		t.Fatalf("expected plan row to remain, got nothing")
	}
	if final.Tier != TierBusiness {
		t.Errorf("tier: got %q, want %q", final.Tier, TierBusiness)
	}
	if final.MaxStorageBytes == nil || *final.MaxStorageBytes != customStorage {
		t.Errorf("max_storage_bytes: got %v, want %d", final.MaxStorageBytes, customStorage)
	}
	if final.MaxUsers == nil || *final.MaxUsers != customUsers {
		t.Errorf("max_users: got %v, want %d", final.MaxUsers, customUsers)
	}
}

func TestStripeSignatureRejection(t *testing.T) {
	const secret = "whsec_test_secret"
	repo := newFakeRepo()
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
			if calls := len(repo.tierCalls); calls != 0 {
				t.Fatalf("UpdateTier must not be called on signature failure, got %d calls", calls)
			}
		})
	}
}
