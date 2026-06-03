package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// memIPAllowStore is an in-memory workspace.IPAllowStore so the admin
// handlers can be exercised over a real IPAllowService without a
// Postgres connection.
type memIPAllowStore struct {
	mu      sync.Mutex
	enabled map[uuid.UUID]bool
	rules   map[uuid.UUID][]workspace.IPRule
}

func newMemIPAllowStore() *memIPAllowStore {
	return &memIPAllowStore{
		enabled: make(map[uuid.UUID]bool),
		rules:   make(map[uuid.UUID][]workspace.IPRule),
	}
}

func (m *memIPAllowStore) ListRules(_ context.Context, ws uuid.UUID) ([]workspace.IPRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]workspace.IPRule, len(m.rules[ws]))
	copy(out, m.rules[ws])
	return out, nil
}

// AddRule mirrors PostgresIPAllowStore.AddRule's atomic cap +
// uniqueness enforcement so the handler's 409 mappings
// (IP_RULE_CAP_EXCEEDED / DUPLICATE_CIDR) are exercised without
// Postgres.
func (m *memIPAllowStore) AddRule(_ context.Context, rule workspace.IPRule) (workspace.IPRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.rules[rule.WorkspaceID]
	if len(existing) >= workspace.MaxIPRulesPerWorkspace {
		return workspace.IPRule{}, workspace.ErrTooManyRules
	}
	for _, r := range existing {
		if r.CIDR == rule.CIDR {
			return workspace.IPRule{}, workspace.ErrDuplicateCIDR
		}
	}
	if rule.ID == uuid.Nil {
		rule.ID = uuid.New()
	}
	m.rules[rule.WorkspaceID] = append(m.rules[rule.WorkspaceID], rule)
	return rule, nil
}

func (m *memIPAllowStore) RemoveRule(_ context.Context, ws, ruleID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.rules[ws]
	for i, r := range existing {
		if r.ID == ruleID {
			m.rules[ws] = append(existing[:i], existing[i+1:]...)
			return nil
		}
	}
	return workspace.ErrNotFound
}

func (m *memIPAllowStore) IsEnabled(_ context.Context, ws uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled[ws], nil
}

func (m *memIPAllowStore) SetEnabled(_ context.Context, ws uuid.UUID, enabled bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.enabled[ws]
	m.enabled[ws] = enabled
	return prev, nil
}

// newIPAllowTestHandler returns a Handler with only the IP-allowlist
// service wired, plus the chi router and a workspace/user-bound
// request context factory.
func newIPAllowTestHandler(t *testing.T, store workspace.IPAllowStore) (*Handler, chi.Router, uuid.UUID) {
	t.Helper()
	svc := workspace.NewIPAllowService(store, nil)
	h := NewHandler(nil, nil, nil, nil).WithIPAllow(svc)
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return h, r, uuid.New()
}

func authedCtxRequest(method, target string, body []byte, ws uuid.UUID) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	ctx := middleware.WithWorkspaceID(req.Context(), ws)
	ctx = middleware.WithUserID(ctx, uuid.New())
	return req.WithContext(ctx)
}

func TestAddIPAllowRule_Success(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)

	body, _ := json.Marshal(addIPAllowRuleRequest{CIDR: "203.0.113.0/24", Label: "office"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodPost, "/ip-allowlist", body, ws))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want %d (body=%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var resp ipRuleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CIDR != "203.0.113.0/24" || resp.Label != "office" {
		t.Fatalf("unexpected rule: %+v", resp)
	}
}

func TestAddIPAllowRule_PrivateRejected(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)

	body, _ := json.Marshal(addIPAllowRuleRequest{CIDR: "10.0.0.0/8"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodPost, "/ip-allowlist", body, ws))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusBadRequest)
	}
	var resp middleware.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != middleware.ErrCodePrivateCIDR {
		t.Fatalf("code: got %q want %q", resp.Code, middleware.ErrCodePrivateCIDR)
	}
}

func TestAddIPAllowRule_DuplicateRejected(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)

	body, _ := json.Marshal(addIPAllowRuleRequest{CIDR: "203.0.113.0/24", Label: "office"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodPost, "/ip-allowlist", body, ws))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first add status: got %d want %d (body=%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// Re-adding the same range (here via a host address that
	// canonicalizes to the same network) must be a 409 DUPLICATE_CIDR.
	body, _ = json.Marshal(addIPAllowRuleRequest{CIDR: "203.0.113.42/24", Label: "office-again"})
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodPost, "/ip-allowlist", body, ws))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate add status: got %d want %d (body=%s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var resp middleware.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != middleware.ErrCodeDuplicateCIDR {
		t.Fatalf("code: got %q want %q", resp.Code, middleware.ErrCodeDuplicateCIDR)
	}
}

func TestAddIPAllowRule_MissingCIDR(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)

	body, _ := json.Marshal(addIPAllowRuleRequest{Label: "no cidr"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodPost, "/ip-allowlist", body, ws))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestListIPAllowRules_ReturnsEnabledAndRules(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)
	store.enabled[ws] = true
	store.rules[ws] = []workspace.IPRule{{ID: uuid.New(), WorkspaceID: ws, CIDR: "203.0.113.0/24", Label: "hq"}}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodGet, "/ip-allowlist", nil, ws))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusOK)
	}
	var resp listIPAllowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled {
		t.Fatalf("expected enabled true")
	}
	if len(resp.Rules) != 1 || resp.Rules[0].CIDR != "203.0.113.0/24" {
		t.Fatalf("unexpected rules: %+v", resp.Rules)
	}
}

func TestRemoveIPAllowRule_SuccessAndNotFound(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)
	ruleID := uuid.New()
	store.rules[ws] = []workspace.IPRule{{ID: ruleID, WorkspaceID: ws, CIDR: "203.0.113.0/24"}}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodDelete, "/ip-allowlist/"+ruleID.String(), nil, ws))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d want %d (body=%s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	// Second delete of the same id is a 404.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodDelete, "/ip-allowlist/"+ruleID.String(), nil, ws))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestRemoveIPAllowRule_InvalidID(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodDelete, "/ip-allowlist/not-a-uuid", nil, ws))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestUpdateIPAllowPolicy_TogglesAndRequiresKey(t *testing.T) {
	store := newMemIPAllowStore()
	_, r, ws := newIPAllowTestHandler(t, store)

	enabled := true
	body, _ := json.Marshal(updateIPAllowPolicyRequest{Enabled: &enabled})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodPatch, "/ip-allowlist/policy", body, ws))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !store.enabled[ws] {
		t.Fatalf("policy not persisted as enabled")
	}

	// Missing key must 400 rather than silently disabling.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodPatch, "/ip-allowlist/policy", []byte(`{}`), ws))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing key status: got %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestIPAllowHandlers_NotWiredReturn501(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil) // no WithIPAllow
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	ws := uuid.New()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authedCtxRequest(http.MethodGet, "/ip-allowlist", nil, ws))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusNotImplemented)
	}
}
