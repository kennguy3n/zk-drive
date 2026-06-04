package notification

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
)

// fakeEndpointValidator implements EndpointValidator. It rejects any
// endpoint whose host is marked blocked, standing in for the
// DNS-resolving *webhooks.URLValidator wired in production. Mutating
// blocked between calls simulates a DNS-rebinding attack.
type fakeEndpointValidator struct {
	blocked map[string]bool
}

func (f *fakeEndpointValidator) Validate(_ context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if f.blocked[u.Hostname()] {
		return nil, fmt.Errorf("host %s resolves to a blocked address", u.Hostname())
	}
	return u, nil
}

// fakeWebPushRepo is an in-memory WebPushRepository keyed by
// (workspace, user, endpoint).
type fakeWebPushRepo struct {
	mu   sync.Mutex
	subs map[string]PushSubscription
}

func newFakeWebPushRepo() *fakeWebPushRepo {
	return &fakeWebPushRepo{subs: map[string]PushSubscription{}}
}

func key(workspaceID, userID uuid.UUID, endpoint string) string {
	return workspaceID.String() + "|" + userID.String() + "|" + endpoint
}

func (f *fakeWebPushRepo) SaveSubscription(_ context.Context, workspaceID, userID uuid.UUID, sub PushSubscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[key(workspaceID, userID, sub.Endpoint)] = sub
	return nil
}

func (f *fakeWebPushRepo) DeleteSubscription(_ context.Context, workspaceID, userID uuid.UUID, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, key(workspaceID, userID, endpoint))
	return nil
}

func (f *fakeWebPushRepo) ListSubscriptions(_ context.Context, workspaceID, userID uuid.UUID) ([]PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := workspaceID.String() + "|" + userID.String() + "|"
	var out []PushSubscription
	for k, v := range f.subs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeWebPushRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// stubHTTPClient records requests and returns a canned status code.
type stubHTTPClient struct {
	mu     sync.Mutex
	calls  int
	status int
}

func (s *stubHTTPClient) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Header:     make(http.Header),
	}, nil
}

// testSubscription returns a PushSubscription with cryptographically
// valid p256dh / auth keys so webpush-go's RFC 8291 payload encryption
// succeeds and the request actually reaches the HTTP client.
func testSubscription(t *testing.T, endpoint string) PushSubscription {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate subscription key: %v", err)
	}
	pub := priv.PublicKey().Bytes()
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth: %v", err)
	}
	return PushSubscription{
		Endpoint: endpoint,
		P256dh:   base64.RawURLEncoding.EncodeToString(pub),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}
}

func newTestService(t *testing.T, repo WebPushRepository, status int) (*WebPushService, *stubHTTPClient) {
	t.Helper()
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate vapid keys: %v", err)
	}
	stub := &stubHTTPClient{status: status}
	svc := NewWebPushService(repo, pub, priv).WithHTTPClient(stub)
	if svc == nil {
		t.Fatal("NewWebPushService returned nil with valid keys")
	}
	return svc, stub
}

func TestNewWebPushService_DisabledWhenKeysMissing(t *testing.T) {
	repo := newFakeWebPushRepo()
	if NewWebPushService(repo, "", "priv") != nil {
		t.Error("expected nil service when public key empty")
	}
	if NewWebPushService(repo, "pub", "") != nil {
		t.Error("expected nil service when private key empty")
	}
	if NewWebPushService(nil, "pub", "priv") != nil {
		t.Error("expected nil service when repo nil")
	}
}

func TestWebPushService_NilIsNoop(t *testing.T) {
	var svc *WebPushService
	ctx := context.Background()
	if err := svc.Subscribe(ctx, uuid.New(), uuid.New(), PushSubscription{}); err != nil {
		t.Errorf("nil Subscribe: %v", err)
	}
	if err := svc.Unsubscribe(ctx, uuid.New(), uuid.New(), "e"); err != nil {
		t.Errorf("nil Unsubscribe: %v", err)
	}
	if err := svc.Send(ctx, uuid.New(), uuid.New(), NotificationPayload{}); err != nil {
		t.Errorf("nil Send: %v", err)
	}
	if svc.PublicKey() != "" {
		t.Error("nil PublicKey should be empty")
	}
}

func TestWebPushService_SubscribeValidation(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, _ := newTestService(t, repo, http.StatusCreated)
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()

	if err := svc.Subscribe(ctx, ws, user, PushSubscription{Endpoint: "e"}); err == nil {
		t.Error("expected error when keys missing")
	}
	if repo.count() != 0 {
		t.Errorf("expected no subscription stored, got %d", repo.count())
	}

	sub := testSubscription(t, "https://push.example.com/abc")
	if err := svc.Subscribe(ctx, ws, user, sub); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if repo.count() != 1 {
		t.Errorf("expected 1 subscription, got %d", repo.count())
	}
}

func TestWebPushService_SubscribeRejectsUnsafeEndpoints(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, _ := newTestService(t, repo, http.StatusCreated)
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()

	bad := []string{
		"http://push.example.com/abc",    // not https
		"https://localhost/abc",          // localhost
		"https://127.0.0.1/abc",          // loopback
		"https://10.0.0.5/abc",           // RFC 1918 private
		"https://192.168.1.1/abc",        // RFC 1918 private
		"https://169.254.169.254/latest", // cloud metadata (link-local)
		"https://0.0.0.0/abc",            // unspecified
		"not-a-url",                      // no https scheme
	}
	for _, endpoint := range bad {
		sub := testSubscription(t, endpoint)
		// Must surface ErrInvalidSubscription so the HTTP layer maps it
		// to 400 (not 500 with an ERROR log) — see writeServiceError.
		if err := svc.Subscribe(ctx, ws, user, sub); !errors.Is(err, ErrInvalidSubscription) {
			t.Errorf("expected ErrInvalidSubscription for endpoint %q, got %v", endpoint, err)
		}
	}
	if repo.count() != 0 {
		t.Errorf("expected no unsafe subscription stored, got %d", repo.count())
	}

	// A normal public https push endpoint is still accepted.
	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://fcm.googleapis.com/fcm/send/abc")); err != nil {
		t.Errorf("expected public https endpoint to be accepted: %v", err)
	}
}

func TestWebPushService_RejectsOverLongEndpoint(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, _ := newTestService(t, repo, http.StatusCreated)
	ctx := context.Background()

	long := "https://push.example.com/" + strings.Repeat("a", maxPushEndpointLen)
	err := svc.Subscribe(ctx, uuid.New(), uuid.New(), testSubscription(t, long))
	if !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("expected ErrInvalidSubscription for over-long endpoint, got %v", err)
	}
	if repo.count() != 0 {
		t.Errorf("over-long endpoint should not be stored, got %d", repo.count())
	}
}

// TestWebPushService_DeliverFallbackValidationWithoutValidator proves
// that when no EndpointValidator is injected, delivery still runs the
// literal-IP fallback check (validatePushEndpoint) before POSTing —
// matching subscribe-time behaviour rather than skipping pre-send
// validation entirely. We seed the repo directly (bypassing Subscribe's
// own validation) with a loopback endpoint to simulate a row that
// should never be delivered to, then assert Send makes no HTTP call and
// leaves the row intact (a validation failure is not a 410, so we don't
// prune it).
func TestWebPushService_DeliverFallbackValidationWithoutValidator(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, stub := newTestService(t, repo, http.StatusCreated) // no validator injected
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()

	// Seed straight through the repo so Subscribe's validation is bypassed.
	if err := repo.SaveSubscription(ctx, ws, user, testSubscription(t, "https://127.0.0.1/x")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.Send(ctx, ws, user, NotificationPayload{Title: "T", Body: "B"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("disallowed endpoint must not be POSTed to, got %d HTTP calls", stub.calls)
	}
	if repo.count() != 1 {
		t.Errorf("validation failure (not a 410) must not prune the row, got count %d", repo.count())
	}
}

// TestWebPushService_WithEndpointValidatorTypedNilFallsBack proves that
// a typed-nil concrete validator (a nil *fakeEndpointValidator wrapped
// in the EndpointValidator interface) is normalised to plain nil by the
// setter, so the literal-IP fallback engages instead of dispatching
// Validate on a nil receiver (which would NPE). Without the
// typednil.IsTypedNil guard, `s.validator != nil` would be true and the
// first Subscribe would panic.
func TestWebPushService_WithEndpointValidatorTypedNilFallsBack(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, _ := newTestService(t, repo, http.StatusCreated)
	var typedNil *fakeEndpointValidator // nil concrete pointer
	svc.WithEndpointValidator(typedNil) // wrapped in EndpointValidator interface
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()

	// Fallback literal-IP check must still reject a loopback endpoint
	// (rather than panicking on the typed-nil validator).
	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://127.0.0.1/x")); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("expected fallback to reject loopback, got %v", err)
	}
	// A normal public https endpoint is still accepted via the fallback.
	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://fcm.googleapis.com/fcm/send/abc")); err != nil {
		t.Errorf("expected public endpoint accepted via fallback, got %v", err)
	}
}

// TestWebPushService_InjectedValidatorGatesSubscribeAndDeliver proves
// the injected DNS-resolving validator runs both at subscribe time and
// again before each delivery — the latter being the DNS-rebinding
// defence (a host that was public at subscribe time but is later
// repointed at an internal address is never POSTed to).
func TestWebPushService_InjectedValidatorGatesSubscribeAndDeliver(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, stub := newTestService(t, repo, http.StatusCreated)
	validator := &fakeEndpointValidator{blocked: map[string]bool{"evil.internal": true}}
	svc.WithEndpointValidator(validator)
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()

	// Subscribe to a validator-blocked host is rejected (and not stored).
	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://evil.internal/x")); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("expected ErrInvalidSubscription for blocked host, got %v", err)
	}
	if repo.count() != 0 {
		t.Errorf("blocked subscription must not be stored, got %d", repo.count())
	}

	// A good host subscribes fine.
	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://fcm.googleapis.com/fcm/send/x")); err != nil {
		t.Fatalf("good subscribe: %v", err)
	}

	// Simulate DNS rebinding: the stored host now resolves to a blocked
	// address. deliver must re-validate and skip the send entirely.
	validator.blocked["fcm.googleapis.com"] = true
	if err := svc.Send(ctx, ws, user, NotificationPayload{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("expected 0 deliveries after rebinding, got %d", stub.calls)
	}
	// The subscription is NOT pruned — a rebinding block is not a 410/404.
	if repo.count() != 1 {
		t.Errorf("expected subscription retained, got %d", repo.count())
	}
}

func TestWebPushService_SendDeliversToAllSubscriptions(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, stub := newTestService(t, repo, http.StatusCreated)
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()

	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://push.example.com/a")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://push.example.com/b")); err != nil {
		t.Fatal(err)
	}

	if err := svc.Send(ctx, ws, user, NotificationPayload{Title: "Hi", Body: "There"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.calls != 2 {
		t.Errorf("expected 2 push deliveries, got %d", stub.calls)
	}
	if repo.count() != 2 {
		t.Errorf("expected subscriptions retained on success, got %d", repo.count())
	}
}

func TestWebPushService_SendRemovesSubscriptionOn410(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, stub := newTestService(t, repo, http.StatusGone)
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()

	if err := svc.Subscribe(ctx, ws, user, testSubscription(t, "https://push.example.com/gone")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Send(ctx, ws, user, NotificationPayload{Title: "Hi", Body: "There"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected 1 delivery attempt, got %d", stub.calls)
	}
	if repo.count() != 0 {
		t.Errorf("expected expired subscription auto-removed, got %d", repo.count())
	}
}

func TestWebPushService_SendNoSubscriptionsIsNoop(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, stub := newTestService(t, repo, http.StatusCreated)
	if err := svc.Send(context.Background(), uuid.New(), uuid.New(), NotificationPayload{Title: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("expected no deliveries, got %d", stub.calls)
	}
}

func TestWebPushService_Unsubscribe(t *testing.T) {
	repo := newFakeWebPushRepo()
	svc, _ := newTestService(t, repo, http.StatusCreated)
	ctx := context.Background()
	ws, user := uuid.New(), uuid.New()
	sub := testSubscription(t, "https://push.example.com/x")
	if err := svc.Subscribe(ctx, ws, user, sub); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unsubscribe(ctx, ws, user, ""); !errors.Is(err, ErrInvalidSubscription) {
		t.Errorf("empty endpoint should wrap ErrInvalidSubscription (maps to 400), got %v", err)
	}
	if err := svc.Unsubscribe(ctx, ws, user, sub.Endpoint); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if repo.count() != 0 {
		t.Errorf("expected subscription removed, got %d", repo.count())
	}
}

// TestWebPushPublisher_FansToPushForOfflineUser verifies the publisher
// decorator delegates to the inner publisher and only pushes to users
// without a live WebSocket connection.
func TestWebPushPublisher_FansToPushForOfflineUser(t *testing.T) {
	ctx := context.Background()
	ws, online, offline := uuid.New(), uuid.New(), uuid.New()

	inner := &recordingPublisher{}
	conns := connSet{online: true}
	push := &recordingPushSender{signal: make(chan struct{}, 1)}
	pub := NewWebPushPublisher(inner, conns, push)

	evt := Event{Type: "notification", Payload: &Notification{Title: "T", Body: "B", Type: "share_link.created"}}

	if err := pub.Publish(ctx, ws, online, evt); err != nil {
		t.Fatalf("Publish online: %v", err)
	}
	if err := pub.Publish(ctx, ws, offline, evt); err != nil {
		t.Fatalf("Publish offline: %v", err)
	}

	// Push fans out asynchronously; wait for the single offline delivery.
	select {
	case <-push.signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async push delivery")
	}

	if inner.calls != 2 {
		t.Errorf("expected inner publish for both users, got %d", inner.calls)
	}
	sent := push.sentUsers()
	if len(sent) != 1 {
		t.Fatalf("expected 1 push (offline only), got %d", len(sent))
	}
	if sent[0] != offline {
		t.Errorf("expected push to offline user %s, got %s", offline, sent[0])
	}
}

func TestWebPushPublisher_IgnoresNonNotificationEvents(t *testing.T) {
	inner := &recordingPublisher{}
	push := &recordingPushSender{}
	pub := NewWebPushPublisher(inner, connSet{}, push)
	if err := pub.Publish(context.Background(), uuid.New(), uuid.New(), Event{Type: "change", Payload: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	// Non-notification events short-circuit before the async fan-out,
	// so no goroutine is spawned and the snapshot is stable.
	if sent := push.sentUsers(); len(sent) != 0 {
		t.Errorf("expected no push for non-notification event, got %d", len(sent))
	}
}

// TestPushPayloadFromEvent_DeepLink verifies the click-through URL is
// derived from the notification's resource: folder / document resources
// map to their SPA route, while resource types without a dedicated route
// (and non-notification events) leave URL empty so the service worker
// applies its /drive fallback.
func TestPushPayloadFromEvent_DeepLink(t *testing.T) {
	folderID := uuid.New()
	docID := uuid.New()
	otherID := uuid.New()
	folderType := "folder"
	docType := "document"
	shareType := "share_link"

	cases := []struct {
		name    string
		event   Event
		wantOK  bool
		wantURL string
	}{
		{
			name:    "folder resource deep-links to folder route",
			event:   Event{Type: "notification", Payload: &Notification{Title: "T", Body: "B", Type: "x", ResourceType: &folderType, ResourceID: &folderID}},
			wantOK:  true,
			wantURL: "/drive/folder/" + folderID.String(),
		},
		{
			name:    "document resource deep-links to document route",
			event:   Event{Type: "notification", Payload: &Notification{Title: "T", Body: "B", Type: "x", ResourceType: &docType, ResourceID: &docID}},
			wantOK:  true,
			wantURL: "/drive/document/" + docID.String(),
		},
		{
			name:    "routeless resource type leaves URL empty (SW falls back to /drive)",
			event:   Event{Type: "notification", Payload: &Notification{Title: "T", Body: "B", Type: "x", ResourceType: &shareType, ResourceID: &otherID}},
			wantOK:  true,
			wantURL: "",
		},
		{
			name:    "missing resource leaves URL empty",
			event:   Event{Type: "notification", Payload: &Notification{Title: "T", Body: "B", Type: "x"}},
			wantOK:  true,
			wantURL: "",
		},
		{
			name:   "non-notification event is not surfaced",
			event:  Event{Type: "change", Payload: map[string]string{"k": "v"}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, ok := pushPayloadFromEvent(tc.event)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if payload.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", payload.URL, tc.wantURL)
			}
		})
	}
}

// TestWebPushPublisher_TypedNilPushDegradesGracefully proves that a
// typed-nil *WebPushService passed as the PushSender interface is
// normalised to a plain-nil field, so Publish short-circuits to the
// inner publisher instead of spawning a goroutine that dispatches on a
// nil receiver. (A plain `p.push == nil` check would be false for a
// typed nil, the classic Go interface-nil trap.)
func TestWebPushPublisher_TypedNilPushDegradesGracefully(t *testing.T) {
	inner := &recordingPublisher{}
	var nilSvc *WebPushService // typed nil implementing PushSender
	pub := NewWebPushPublisher(inner, connSet{}, nilSvc)
	if pub.push != nil {
		t.Fatalf("typed-nil PushSender should be normalised to nil, got %#v", pub.push)
	}

	evt := Event{Type: "notification", Payload: &Notification{Title: "T", Body: "B"}}
	if err := pub.Publish(context.Background(), uuid.New(), uuid.New(), evt); err != nil {
		t.Fatalf("Publish with typed-nil push: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("inner publish should still run, got %d calls", inner.calls)
	}
}

// TestWebPushPublisher_WaitGroupTracksDelivery proves that, when a
// WaitGroup is registered, the detached push goroutine is tracked: a
// Wait() unblocks only once delivery has completed. This is what lets
// graceful shutdown drain in-flight pushes before the pool closes.
func TestWebPushPublisher_WaitGroupTracksDelivery(t *testing.T) {
	inner := &recordingPublisher{}
	release := make(chan struct{})
	push := &blockingPushSender{release: release}
	var wg sync.WaitGroup
	pub := NewWebPushPublisher(inner, connSet{}, push).WithWaitGroup(&wg)

	evt := Event{Type: "notification", Payload: &Notification{Title: "T", Body: "B"}}
	if err := pub.Publish(context.Background(), uuid.New(), uuid.New(), evt); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Wait must still be blocked while the push goroutine is in-flight.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("WaitGroup unblocked before push delivery completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(release) // let the push finish
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitGroup did not unblock after push delivery completed")
	}
	if got := push.count(); got != 1 {
		t.Errorf("expected 1 delivery, got %d", got)
	}
}

type recordingPublisher struct{ calls int }

func (r *recordingPublisher) Publish(_ context.Context, _, _ uuid.UUID, _ Event) error {
	r.calls++
	return nil
}

// recordingPushSender records the users a push was delivered to. The
// WebPushPublisher fans push out in a detached goroutine, so Send may
// run after Publish returns; the mutex + signal channel let tests wait
// for delivery deterministically instead of sleeping.
type recordingPushSender struct {
	mu     sync.Mutex
	sent   []uuid.UUID
	signal chan struct{}
}

func (r *recordingPushSender) Send(_ context.Context, _, userID uuid.UUID, _ NotificationPayload) error {
	r.mu.Lock()
	r.sent = append(r.sent, userID)
	r.mu.Unlock()
	if r.signal != nil {
		r.signal <- struct{}{}
	}
	return nil
}

// sentUsers returns a snapshot of the recorded recipients.
func (r *recordingPushSender) sentUsers() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.sent...)
}

// connSet reports a user as connected when present in the map.
type connSet map[uuid.UUID]bool

func (c connSet) IsConnected(_, userID uuid.UUID) bool { return c[userID] }

// blockingPushSender blocks inside Send until release is closed, so a
// test can observe the WaitGroup counter while a push is in-flight.
type blockingPushSender struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *blockingPushSender) Send(_ context.Context, _, _ uuid.UUID, _ NotificationPayload) error {
	<-b.release
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	return nil
}

func (b *blockingPushSender) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}
