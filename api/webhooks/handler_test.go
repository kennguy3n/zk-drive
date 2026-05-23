package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// fakeRepo is an in-memory webhooks.Repository for handler tests.
// Per-workspace cap is enforced like the production Postgres
// implementation so the cap-related tests exercise the real code
// path through h.Create.
type fakeRepo struct {
	mu         sync.Mutex
	subs       []*webhooks.Subscription
	deliveries []*webhooks.Delivery
	failCreate error
	failList   error
	failGet    error
	failDelete error
}

func (f *fakeRepo) Create(ctx context.Context, s *webhooks.Subscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate != nil {
		return f.failCreate
	}
	active := 0
	for _, e := range f.subs {
		if e.WorkspaceID == s.WorkspaceID && e.Active {
			active++
		}
	}
	if active >= webhooks.MaxSubscriptionsPerWorkspace {
		return webhooks.ErrSubscriptionCapReached
	}
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Secret == "" {
		// Mirror the production layer: a real secret is generated
		// on insert. 64 hex chars = 32 random bytes, the
		// SecretByteLength used by repository.Create.
		s.Secret = strings.Repeat("a", 64)
	}
	s.Active = true
	s.CreatedAt = time.Now().UTC()
	s.UpdatedAt = s.CreatedAt
	cp := *s
	f.subs = append(f.subs, &cp)
	*s = cp
	return nil
}

func (f *fakeRepo) GetByID(ctx context.Context, workspaceID, id uuid.UUID) (*webhooks.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		return nil, f.failGet
	}
	for _, s := range f.subs {
		if s.WorkspaceID == workspaceID && s.ID == id {
			cp := *s
			cp.Secret = ""
			return &cp, nil
		}
	}
	return nil, webhooks.ErrSubscriptionNotFound
}

func (f *fakeRepo) List(ctx context.Context, workspaceID uuid.UUID) ([]*webhooks.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList != nil {
		return nil, f.failList
	}
	out := []*webhooks.Subscription{}
	for _, s := range f.subs {
		if s.WorkspaceID == workspaceID {
			cp := *s
			cp.Secret = ""
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeRepo) ListActiveForEvent(ctx context.Context, workspaceID uuid.UUID, t webhooks.EventType) ([]*webhooks.Subscription, error) {
	return nil, nil
}

func (f *fakeRepo) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete != nil {
		return f.failDelete
	}
	for i, s := range f.subs {
		if s.WorkspaceID == workspaceID && s.ID == id {
			f.subs = append(f.subs[:i], f.subs[i+1:]...)
			return nil
		}
	}
	return webhooks.ErrSubscriptionNotFound
}

func (f *fakeRepo) UpdateAttempt(ctx context.Context, workspaceID, subID uuid.UUID, outcome webhooks.DeliveryOutcome, at time.Time) error {
	return nil
}

func (f *fakeRepo) SetActive(ctx context.Context, workspaceID, id uuid.UUID, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subs {
		if s.WorkspaceID == workspaceID && s.ID == id {
			s.Active = active
			now := time.Now().UTC()
			s.UpdatedAt = now
			if active {
				s.ConsecutiveFailures = 0
				s.AutoPausedAt = nil
			}
			return nil
		}
	}
	return webhooks.ErrSubscriptionNotFound
}

func (f *fakeRepo) InsertDelivery(ctx context.Context, d *webhooks.Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *d
	f.deliveries = append(f.deliveries, &cp)
	return nil
}

func (f *fakeRepo) ListDeliveries(ctx context.Context, workspaceID, subID uuid.UUID, limit int) ([]*webhooks.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*webhooks.Delivery{}
	for _, d := range f.deliveries {
		if d.WorkspaceID == workspaceID && d.SubscriptionID == subID {
			cp := *d
			out = append(out, &cp)
		}
	}
	return out, nil
}

// testValidator wires a URLValidator with a resolver that maps any
// hostname to a routable public IP, so SSRF checks pass without
// touching DNS.
func testValidator() *webhooks.URLValidator {
	v := webhooks.NewURLValidator()
	v.Resolver = &fakeResolver{ipForHost: net.ParseIP("1.1.1.1")}
	return v
}

type fakeResolver struct {
	ipForHost net.IP
	err       error
}

func (f *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []net.IPAddr{{IP: f.ipForHost}}, nil
}

// newAdminCtx returns a context wired with the standard admin
// auth attributes (workspace_id, user_id, role=admin) that the
// production AuthMiddleware would have set from a verified JWT.
func newAdminCtx(workspaceID, userID uuid.UUID) context.Context {
	ctx := middleware.WithWorkspaceID(context.Background(), workspaceID)
	ctx = middleware.WithUserID(ctx, userID)
	ctx = middleware.WithRole(ctx, user.RoleAdmin)
	return ctx
}

// newRouter constructs a chi router and mounts the webhook handler
// at /webhooks. Tests issue requests with WithContext on the
// http.Request so the URL params resolve via chi's pattern matcher.
func newRouter(h *Handler) *chi.Mux {
	r := chi.NewMux()
	r.Route("/webhooks", func(r chi.Router) {
		h.RegisterRoutes(r)
	})
	return r
}

func TestHandler_Create_Success(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	workspaceID, userID := uuid.New(), uuid.New()
	body := `{"url":"https://hooks.example.com/x","event_type":"file.upload.confirmed","description":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/", bytes.NewBufferString(body))
	req = req.WithContext(newAdminCtx(workspaceID, userID))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got=%d want=201 body=%s", rec.Code, rec.Body.String())
	}
	var got subscriptionView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got.Secret == "" {
		t.Errorf("create response must include secret exactly once")
	}
	if got.WorkspaceID != workspaceID {
		t.Errorf("workspace_id: got=%s want=%s", got.WorkspaceID, workspaceID)
	}
}

func TestHandler_Create_RequiresAdmin(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	// member role — not admin.
	ctx := middleware.WithWorkspaceID(context.Background(), uuid.New())
	ctx = middleware.WithUserID(ctx, uuid.New())
	ctx = middleware.WithRole(ctx, user.RoleMember)
	body := `{"url":"https://hooks.example.com/x","event_type":"file.upload.confirmed"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/", bytes.NewBufferString(body))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got=%d want=403", rec.Code)
	}
}

func TestHandler_Create_RejectsInvalidURL(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	body := `{"url":"ftp://hooks.example.com/x","event_type":"file.upload.confirmed"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/", bytes.NewBufferString(body))
	req = req.WithContext(newAdminCtx(uuid.New(), uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Create_RejectsUnknownEventType(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	body := `{"url":"https://hooks.example.com/x","event_type":"file.totally_unknown"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/", bytes.NewBufferString(body))
	req = req.WithContext(newAdminCtx(uuid.New(), uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Create_CapReached(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	workspaceID := uuid.New()
	// Pre-populate to the cap.
	for i := 0; i < webhooks.MaxSubscriptionsPerWorkspace; i++ {
		repo.subs = append(repo.subs, &webhooks.Subscription{
			ID: uuid.New(), WorkspaceID: workspaceID, Active: true,
		})
	}
	body := `{"url":"https://hooks.example.com/x","event_type":"file.upload.confirmed"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/", bytes.NewBufferString(body))
	req = req.WithContext(newAdminCtx(workspaceID, uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got=%d want=409 body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandler_List_HidesSecret(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	workspaceID := uuid.New()
	repo.subs = []*webhooks.Subscription{
		{ID: uuid.New(), WorkspaceID: workspaceID, EventType: webhooks.EventFileUploadConfirmed, URL: "https://x.example.com", Secret: "should-never-be-returned", Active: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}
	req := httptest.NewRequest(http.MethodGet, "/webhooks/", nil)
	req = req.WithContext(newAdminCtx(workspaceID, uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "should-never-be-returned") {
		t.Errorf("secret leaked in list response: %s", rec.Body.String())
	}
}

func TestHandler_Delete(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	workspaceID, id := uuid.New(), uuid.New()
	repo.subs = []*webhooks.Subscription{
		{ID: id, WorkspaceID: workspaceID, Active: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}
	req := httptest.NewRequest(http.MethodDelete, "/webhooks/"+id.String(), nil)
	req = req.WithContext(newAdminCtx(workspaceID, uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got=%d want=204 body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.subs) != 0 {
		t.Errorf("repo.subs after delete: got=%d want=0", len(repo.subs))
	}
}

func TestHandler_Delete_NotFound(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	req := httptest.NewRequest(http.MethodDelete, "/webhooks/"+uuid.New().String(), nil)
	req = req.WithContext(newAdminCtx(uuid.New(), uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got=%d want=404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Resume_ReactivatesSubscription(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	workspaceID, id := uuid.New(), uuid.New()
	now := time.Now().UTC()
	repo.subs = []*webhooks.Subscription{
		{
			ID: id, WorkspaceID: workspaceID,
			EventType: webhooks.EventFileUploadConfirmed, URL: "https://x.example.com",
			Active: false, ConsecutiveFailures: 50, AutoPausedAt: &now,
			CreatedAt: now, UpdatedAt: now,
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+id.String()+"/resume", nil)
	req = req.WithContext(newAdminCtx(workspaceID, uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("status: got=%d want=200/204 body=%s", rec.Code, rec.Body.String())
	}
	if !repo.subs[0].Active {
		t.Errorf("subscription not re-activated")
	}
	if repo.subs[0].ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures not reset: got=%d", repo.subs[0].ConsecutiveFailures)
	}
}

func TestHandler_Test_RequiresPublisher(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	workspaceID, id := uuid.New(), uuid.New()
	repo.subs = []*webhooks.Subscription{
		{ID: id, WorkspaceID: workspaceID, EventType: webhooks.EventFileUploadConfirmed, URL: "https://x", Active: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+id.String()+"/test", nil)
	req = req.WithContext(newAdminCtx(workspaceID, uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got=%d want=503 body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Create_FailsCleanlyOnRepoError(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{failCreate: errors.New("db is down")}
	h := NewHandler(repo).WithValidator(testValidator())
	r := newRouter(h)
	body := `{"url":"https://hooks.example.com/x","event_type":"file.upload.confirmed"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/", bytes.NewBufferString(body))
	req = req.WithContext(newAdminCtx(uuid.New(), uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got=%d want=500 body=%s", rec.Code, rec.Body.String())
	}
}
