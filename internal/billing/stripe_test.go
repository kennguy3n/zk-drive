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
