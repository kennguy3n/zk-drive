package webhooks

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// fakeRepo is an in-memory Repository implementation used by the
// worker tests. It implements the surface area Consume / deliverOne
// actually touch (List, ListActiveForEvent, InsertDelivery,
// UpdateAttempt) and panics on the rest so tests catch unexpected
// repository calls instead of silently returning empty slices.
type fakeRepo struct {
	mu              sync.Mutex
	subs            []*Subscription
	deliveries      []*Delivery
	attempts        []DeliveryOutcome
	failOnList      bool
	failOnInsert    bool
	failOnUpdate    bool
}

func (f *fakeRepo) Create(ctx context.Context, s *Subscription) error { panic("unused in worker test") }
func (f *fakeRepo) GetByID(ctx context.Context, workspaceID, id uuid.UUID) (*Subscription, error) {
	panic("unused in worker test")
}
func (f *fakeRepo) List(ctx context.Context, workspaceID uuid.UUID) ([]*Subscription, error) {
	panic("unused in worker test")
}
func (f *fakeRepo) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	panic("unused in worker test")
}
func (f *fakeRepo) SetActive(ctx context.Context, workspaceID, id uuid.UUID, active bool) error {
	panic("unused in worker test")
}
func (f *fakeRepo) ListDeliveries(ctx context.Context, workspaceID, subID uuid.UUID, limit int) ([]*Delivery, error) {
	panic("unused in worker test")
}

func (f *fakeRepo) ListActiveForEvent(ctx context.Context, workspaceID uuid.UUID, t EventType) ([]*Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnList {
		return nil, errFake
	}
	out := make([]*Subscription, 0, len(f.subs))
	for _, s := range f.subs {
		if s.WorkspaceID == workspaceID && s.EventType == t && s.Active {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeRepo) InsertDelivery(ctx context.Context, d *Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnInsert {
		return errFake
	}
	cp := *d
	f.deliveries = append(f.deliveries, &cp)
	return nil
}

func (f *fakeRepo) UpdateAttempt(ctx context.Context, workspaceID, subID uuid.UUID, outcome DeliveryOutcome, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnUpdate {
		return errFake
	}
	f.attempts = append(f.attempts, outcome)
	return nil
}

// errFake stand-in error for repository-layer failure simulation.
var errFake = &fakeError{msg: "fake repository error"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

type recorder struct {
	calls atomic.Int64
}

func (r *recorder) RecordWebhookDelivery(outcome string, statusCode int, d time.Duration) {
	r.calls.Add(1)
}

// newTestWorker builds a worker pointing at an httptest server. The
// validator is wired to allow loopback so the deliveries actually
// reach the in-process server.
func newTestWorker(t *testing.T, srv *httptest.Server, repo Repository) *DeliveryWorker {
	t.Helper()
	v := loopbackValidator(t, srv)
	client := NewDeliveryClient(v, 5*time.Second)
	w, err := NewDeliveryWorker(repo, client, &recorder{})
	if err != nil {
		t.Fatalf("NewDeliveryWorker: %v", err)
	}
	w.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return w
}

func makeEvent(t EventType, workspaceID uuid.UUID) Event {
	raw, _ := json.Marshal(map[string]any{"test": true})
	return NewEvent(t, workspaceID, nil, raw)
}

func makeSub(workspaceID uuid.UUID, url string, t EventType) *Subscription {
	return &Subscription{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		URL:         url,
		EventType:   t,
		Active:      true,
		Secret:      secret32,
	}
}

func TestWorker_Consume_SkipNoSubscribers(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	repo := &fakeRepo{}
	w := newTestWorker(t, srv, repo)
	ev := makeEvent(EventFileUploadConfirmed, uuid.New())
	body, _ := json.Marshal(ev)
	msg := &nats.Msg{Data: body, Subject: SubjectEvents}
	if got := w.Consume(context.Background(), msg); got != "skip" {
		t.Fatalf("Consume: got=%q want=skip", got)
	}
	if len(repo.deliveries) != 0 {
		t.Errorf("no subscribers should produce 0 deliveries, got %d", len(repo.deliveries))
	}
}

func TestWorker_Consume_FanOutSuccess(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	workspaceID := uuid.New()
	repo := &fakeRepo{
		subs: []*Subscription{
			makeSub(workspaceID, srv.URL, EventFileUploadConfirmed),
			makeSub(workspaceID, srv.URL, EventFileUploadConfirmed),
			makeSub(workspaceID, srv.URL, EventFileDeleted), // wrong type — should NOT be hit
		},
	}
	w := newTestWorker(t, srv, repo)
	ev := makeEvent(EventFileUploadConfirmed, workspaceID)
	body, _ := json.Marshal(ev)
	msg := &nats.Msg{Data: body, Subject: SubjectEvents}
	if got := w.Consume(context.Background(), msg); got != "ok" {
		t.Fatalf("Consume: got=%q want=ok", got)
	}
	if hits.Load() != 2 {
		t.Fatalf("HTTP hits: got=%d want=2", hits.Load())
	}
	if len(repo.deliveries) != 2 {
		t.Fatalf("repo.deliveries: got=%d want=2", len(repo.deliveries))
	}
	for _, d := range repo.deliveries {
		if d.Outcome != OutcomeSuccess {
			t.Errorf("delivery outcome: got=%s want=%s", d.Outcome, OutcomeSuccess)
		}
	}
}

func TestWorker_Consume_PartialFailure(t *testing.T) {
	t.Parallel()
	// One subscriber 2xxs, one 500s — Consume must return "error"
	// so JetStream re-queues, AND the 500 must be recorded as
	// outcome=http_error.
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv200.Close()
	workspaceID := uuid.New()
	repo := &fakeRepo{
		subs: []*Subscription{
			makeSub(workspaceID, srv200.URL, EventPermissionGranted),
			makeSub(workspaceID, srv500.URL, EventPermissionGranted),
		},
	}
	// Validator covers both loopback servers (they're both 127.0.0.1).
	host, _, _ := net.SplitHostPort(mustHost(t, srv200.URL))
	v := NewURLValidator()
	v.AllowHTTP = true
	v.AllowLoopback = true
	v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
		host: {{IP: net.ParseIP(host)}},
	}}
	client := NewDeliveryClient(v, 5*time.Second)
	w, _ := NewDeliveryWorker(repo, client, &recorder{})
	w.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	ev := makeEvent(EventPermissionGranted, workspaceID)
	body, _ := json.Marshal(ev)
	msg := &nats.Msg{Data: body, Subject: SubjectEvents}
	got := w.Consume(context.Background(), msg)
	if got != "error" {
		t.Fatalf("Consume: got=%q want=error", got)
	}
	if len(repo.deliveries) != 2 {
		t.Fatalf("repo.deliveries: got=%d want=2", len(repo.deliveries))
	}
	outcomes := map[DeliveryOutcome]int{}
	for _, d := range repo.deliveries {
		outcomes[d.Outcome]++
	}
	if outcomes[OutcomeSuccess] != 1 || outcomes[OutcomeHTTPError] != 1 {
		t.Errorf("outcomes mix: %+v", outcomes)
	}
}

// TestWorker_Consume_NextRetryAtAnchoredPostDelivery pins the
// staleness fix: NextRetryAt on the persisted Delivery row must be
// anchored to the post-delivery "now" (the second w.now() call),
// not the pre-delivery attempt-start ts (the first w.now() call).
// This matters for fan-outs that take non-trivial time: the actual
// JetStream NakWithDelay only fires AFTER the entire fan-out loop,
// so anchoring at the per-attempt start would store a NextRetryAt
// that is up to (N-1) * delivery-time in the past relative to the
// real Nak. The admin UI reads this column directly.
func TestWorker_Consume_NextRetryAtAnchoredPostDelivery(t *testing.T) {
	t.Parallel()
	// 500 server so the delivery records as a retryable
	// http_error outcome and NextRetryAt is populated.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	workspaceID := uuid.New()
	repo := &fakeRepo{subs: []*Subscription{makeSub(workspaceID, srv.URL, EventFileUploadConfirmed)}}

	// Step `now` forward on each call so the first call (ts) and
	// the second call (NextRetryAt anchor) return distinct values.
	// If the worker used ts for NextRetryAt the assertion below
	// would catch the regression: NextRetryAt would be off by the
	// gap between t0 and t1.
	v := loopbackValidator(t, srv)
	client := NewDeliveryClient(v, 5*time.Second)
	w, err := NewDeliveryWorker(repo, client, &recorder{})
	if err != nil {
		t.Fatalf("NewDeliveryWorker: %v", err)
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := t0.Add(3 * time.Second)
	var calls int
	w.now = func() time.Time {
		calls++
		if calls == 1 {
			return t0
		}
		return t1
	}

	ev := makeEvent(EventFileUploadConfirmed, workspaceID)
	body, _ := json.Marshal(ev)
	msg := &nats.Msg{Data: body, Subject: SubjectEvents}
	if got := w.Consume(context.Background(), msg); got != "error" {
		t.Fatalf("Consume: got=%q want=error", got)
	}
	if len(repo.deliveries) != 1 {
		t.Fatalf("repo.deliveries: got=%d want=1", len(repo.deliveries))
	}
	d := repo.deliveries[0]
	if d.NextRetryAt == nil {
		t.Fatalf("NextRetryAt: got=nil want=non-nil for retryable failure")
	}
	// AttemptedAt anchors at the FIRST w.now() call (t0) — that's
	// the moment we started the HTTP attempt.
	if !d.AttemptedAt.Equal(t0) {
		t.Errorf("AttemptedAt: got=%s want=%s (must equal pre-delivery ts)", d.AttemptedAt, t0)
	}
	// NextRetryAt anchors at the SECOND w.now() call (t1) plus the
	// configured backoff for attempt+1. If the regression resurfaces
	// (anchor reverts to ts/t0), the .Equal check fails by exactly
	// the t1-t0 gap.
	wantNR := t1.Add(BackoffDelay(2)).UTC()
	if !d.NextRetryAt.Equal(wantNR) {
		t.Errorf("NextRetryAt: got=%s want=%s (must anchor post-delivery, not at attempt start)", *d.NextRetryAt, wantNR)
	}
}

func TestWorker_Consume_DroppedOnMalformedPayload(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	repo := &fakeRepo{}
	w := newTestWorker(t, srv, repo)
	// Empty body.
	if got := w.Consume(context.Background(), &nats.Msg{Data: nil}); got != "dropped" {
		t.Errorf("empty data: got=%q want=dropped", got)
	}
	// Non-JSON body.
	if got := w.Consume(context.Background(), &nats.Msg{Data: []byte("not-json")}); got != "dropped" {
		t.Errorf("non-json: got=%q want=dropped", got)
	}
	// Missing workspace_id.
	bad, _ := json.Marshal(Event{ID: uuid.New(), Type: EventFileDeleted})
	if got := w.Consume(context.Background(), &nats.Msg{Data: bad}); got != "dropped" {
		t.Errorf("missing workspace: got=%q want=dropped", got)
	}
}

// mustHost is a small helper extracting the hostname portion of a
// URL for the loopback-resolver wiring.
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u.Host
}
