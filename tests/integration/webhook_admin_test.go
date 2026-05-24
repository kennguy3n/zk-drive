package integration

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// TestAdminWebhooks_CRUDLifecycle exercises every route on the
// outbound-webhook subscription admin surface end-to-end against the
// real Postgres PostgresRepository:
//
//	POST   /api/admin/webhooks            -> Create
//	GET    /api/admin/webhooks            -> List
//	GET    /api/admin/webhooks/{id}       -> Get
//	GET    /api/admin/webhooks/{id}/deliveries -> ListDeliveries
//	POST   /api/admin/webhooks/{id}/resume     -> Resume
//	DELETE /api/admin/webhooks/{id}            -> Delete
//
// Closes the round-9 integration-coverage gap flagged by Devin
// Review: the in-process webhookCapture covers the emission paths,
// but the CRUD surface mounted at /api/admin/webhooks had no
// integration coverage — only the handler unit tests exercised it,
// which skips the chi mounting, admin middleware, Postgres
// migrations 028/029, and the secret-masking projection layer.
func TestAdminWebhooks_CRUDLifecycle(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// CREATE — first request returns the secret exactly once.
	status, body := env.httpRequest(http.MethodPost, "/api/admin/webhooks/", tok.Token, map[string]any{
		"url":         "https://hooks.example.com/zk-drive",
		"event_type":  string(webhooks.EventFileUploadConfirmed),
		"description": "integration-test subscription",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var created struct {
		ID                  uuid.UUID `json:"id"`
		WorkspaceID         uuid.UUID `json:"workspace_id"`
		URL                 string    `json:"url"`
		EventType           string    `json:"event_type"`
		Description         string    `json:"description"`
		Secret              string    `json:"secret"`
		Active              bool      `json:"active"`
		ConsecutiveFailures int       `json:"consecutive_failures"`
	}
	env.decodeJSON(body, &created)
	if created.ID == uuid.Nil {
		t.Fatalf("create: empty id, body=%s", string(body))
	}
	if created.Secret == "" {
		t.Errorf("create response MUST include secret exactly once, got empty")
	}
	if created.URL != "https://hooks.example.com/zk-drive" {
		t.Errorf("create URL: got=%q want=https://hooks.example.com/zk-drive", created.URL)
	}
	if created.EventType != string(webhooks.EventFileUploadConfirmed) {
		t.Errorf("create EventType: got=%q want=%s", created.EventType, webhooks.EventFileUploadConfirmed)
	}
	if !created.Active {
		t.Errorf("create Active: got=false want=true (subscriptions start active)")
	}

	// LIST — returns the subscription with secret masked (the
	// "never returned again after create" contract). The handler
	// wraps the slice in {"subscriptions": [...]} so admin
	// clients can pin pagination metadata later without a wire-
	// format break.
	status, body = env.httpRequest(http.MethodGet, "/api/admin/webhooks/", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", status, string(body))
	}
	var listResp struct {
		Subscriptions []struct {
			ID     uuid.UUID `json:"id"`
			Secret string    `json:"secret"`
		} `json:"subscriptions"`
	}
	env.decodeJSON(body, &listResp)
	if len(listResp.Subscriptions) != 1 {
		t.Fatalf("list: got %d subscriptions, want 1", len(listResp.Subscriptions))
	}
	if listResp.Subscriptions[0].ID != created.ID {
		t.Errorf("list ID: got=%s want=%s", listResp.Subscriptions[0].ID, created.ID)
	}
	if listResp.Subscriptions[0].Secret != "" {
		t.Errorf("list MUST mask secret (returned only once on create), got=%q", listResp.Subscriptions[0].Secret)
	}

	// GET — same contract: secret masked on read-back.
	status, body = env.httpRequest(http.MethodGet, "/api/admin/webhooks/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", status, string(body))
	}
	var got struct {
		ID     uuid.UUID `json:"id"`
		Secret string    `json:"secret"`
		Active bool      `json:"active"`
	}
	env.decodeJSON(body, &got)
	if got.ID != created.ID {
		t.Errorf("get ID: got=%s want=%s", got.ID, created.ID)
	}
	if got.Secret != "" {
		t.Errorf("get MUST mask secret, got=%q", got.Secret)
	}

	// LIST DELIVERIES — empty for a brand-new subscription.
	// Handler returns {"deliveries": [...]} (matching the List shape).
	status, body = env.httpRequest(http.MethodGet, "/api/admin/webhooks/"+created.ID.String()+"/deliveries", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list deliveries: status=%d body=%s", status, string(body))
	}
	var deliveriesResp struct {
		Deliveries []any `json:"deliveries"`
	}
	env.decodeJSON(body, &deliveriesResp)
	if len(deliveriesResp.Deliveries) != 0 {
		t.Errorf("list deliveries (fresh sub): got %d, want 0", len(deliveriesResp.Deliveries))
	}

	// RESUME — idempotent on an already-active subscription. The
	// handler returns 204 No Content (consistent with the
	// DeactivateUser pattern in api/admin/handler.go); the
	// invariants the resume action establishes (Active=true,
	// ConsecutiveFailures=0) are observable on the next GET.
	status, body = env.httpRequest(http.MethodPost, "/api/admin/webhooks/"+created.ID.String()+"/resume", tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("resume: status=%d body=%s want=204", status, string(body))
	}
	status, body = env.httpRequest(http.MethodGet, "/api/admin/webhooks/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get after resume: status=%d body=%s", status, string(body))
	}
	var resumed struct {
		Active              bool `json:"active"`
		ConsecutiveFailures int  `json:"consecutive_failures"`
	}
	env.decodeJSON(body, &resumed)
	if !resumed.Active {
		t.Errorf("resume Active: got=false want=true")
	}
	if resumed.ConsecutiveFailures != 0 {
		t.Errorf("resume ConsecutiveFailures: got=%d want=0", resumed.ConsecutiveFailures)
	}

	// DELETE — soft-delete (sets deleted_at); subsequent GET 404s.
	status, _ = env.httpRequest(http.MethodDelete, "/api/admin/webhooks/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete: status=%d", status)
	}
	status, _ = env.httpRequest(http.MethodGet, "/api/admin/webhooks/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("get after delete: status=%d want=404", status)
	}
	// List should now return zero subscriptions (deleted_at filter).
	status, body = env.httpRequest(http.MethodGet, "/api/admin/webhooks/", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list after delete: status=%d body=%s", status, string(body))
	}
	listResp.Subscriptions = nil
	env.decodeJSON(body, &listResp)
	if len(listResp.Subscriptions) != 0 {
		t.Errorf("list after delete: got %d subscriptions, want 0", len(listResp.Subscriptions))
	}
}

// TestAdminWebhooks_CreateRejectsBadInput pins the validation surface:
// schema-invalid URLs, unknown event types, and event types outside
// the AllEventTypes set all return 400 before the row hits Postgres.
func TestAdminWebhooks_CreateRejectsBadInput(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	cases := []struct {
		name    string
		payload map[string]any
	}{
		{
			name:    "non-https scheme rejected",
			payload: map[string]any{"url": "ftp://hooks.example.com/x", "event_type": string(webhooks.EventFileUploadConfirmed)},
		},
		{
			name:    "unknown event type rejected",
			payload: map[string]any{"url": "https://hooks.example.com/x", "event_type": "file.totally_made_up"},
		},
		{
			name:    "empty url rejected",
			payload: map[string]any{"url": "", "event_type": string(webhooks.EventFileUploadConfirmed)},
		},
		{
			name:    "empty event_type rejected",
			payload: map[string]any{"url": "https://hooks.example.com/x", "event_type": ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := env.httpRequest(http.MethodPost, "/api/admin/webhooks/", tok.Token, tc.payload)
			if status != http.StatusBadRequest {
				t.Errorf("expected 400 for %q, got %d", tc.name, status)
			}
		})
	}
}

// TestAdminWebhooks_NonAdminForbidden pins the access-control
// contract: only the workspace's admin role can manage webhook
// subscriptions. A member-role caller gets 403 on every CRUD route.
//
// Note: the admin middleware stack rejects member-role JWTs at
// /api/admin/* with 403, AND the handler's requireAdmin check runs
// as belt-and-suspenders defence (intentional duplication). This test pins the outer
// admin middleware layer; the inner requireAdmin guard is covered
// by handler_test.go's TestHandler_Create_RequiresAdmin.
func TestAdminWebhooks_NonAdminForbidden(t *testing.T) {
	env := setupEnv(t)
	// Admin Alice creates the workspace.
	adminTok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	// Admin invites Bob as a member.
	status, _ := env.httpRequest(http.MethodPost, "/api/admin/users", adminTok.Token, map[string]string{
		"email":    "bob@acme.test",
		"name":     "Bob",
		"password": "pw-bob",
		"role":     "member",
	})
	if status != http.StatusCreated {
		t.Fatalf("invite member: status=%d", status)
	}
	// Bob logs in.
	memberTok := env.login("bob@acme.test", "pw-bob")

	status, _ = env.httpRequest(http.MethodPost, "/api/admin/webhooks/", memberTok.Token, map[string]any{
		"url":        "https://hooks.example.com/x",
		"event_type": string(webhooks.EventFileUploadConfirmed),
	})
	if status != http.StatusForbidden {
		t.Errorf("member POST /api/admin/webhooks: got=%d want=403", status)
	}

	status, _ = env.httpRequest(http.MethodGet, "/api/admin/webhooks/", memberTok.Token, nil)
	if status != http.StatusForbidden {
		t.Errorf("member GET /api/admin/webhooks: got=%d want=403", status)
	}
}

// TestAdminWebhooks_TestEndpointDeliversSynchronously exercises the
// POST /{id}/test route end-to-end: an admin can trigger a synthetic
// delivery against a subscription bound to a real httptest.Server,
// and the dispatch is synchronous (the response body carries the
// outcome instead of just accepting + returning 202). The route is
// the only one that actually walks the DeliveryClient path inside
// the integration harness — every other CRUD route stops at the
// repository layer.
func TestAdminWebhooks_TestEndpointDeliversSynchronously(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Subscriber stub that records the inbound POST so we can
	// assert headers + outcome below.
	var inbound int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound++
		if got := r.Header.Get("X-ZkDrive-Event-Type"); got == "" {
			t.Errorf("subscriber received empty X-ZkDrive-Event-Type")
		}
		if got := r.Header.Get("X-ZkDrive-Event-Id"); got == "" {
			t.Errorf("subscriber received empty X-ZkDrive-Event-Id")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Create the subscription pointing at the loopback stub.
	// AllowLoopback + AllowHTTP on the harness validator (see
	// setup_test.go) make this URL pass SSRF.
	status, body := env.httpRequest(http.MethodPost, "/api/admin/webhooks/", tok.Token, map[string]any{
		"url":        srv.URL + "/hook",
		"event_type": string(webhooks.EventFileUploadConfirmed),
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	env.decodeJSON(body, &created)

	// Fire the test endpoint.
	status, body = env.httpRequest(http.MethodPost, "/api/admin/webhooks/"+created.ID.String()+"/test", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("test endpoint: status=%d body=%s", status, string(body))
	}
	if inbound != 1 {
		t.Errorf("subscriber received %d POSTs, want exactly 1 (test endpoint must isolate to the target subscription)", inbound)
	}
	var outcome struct {
		Outcome    string `json:"outcome"`
		StatusCode int    `json:"status_code"`
	}
	env.decodeJSON(body, &outcome)
	if outcome.Outcome != string(webhooks.OutcomeSuccess) {
		t.Errorf("test outcome: got=%q want=success body=%s", outcome.Outcome, string(body))
	}
	if outcome.StatusCode != http.StatusOK {
		t.Errorf("test status_code: got=%d want=200", outcome.StatusCode)
	}
}
