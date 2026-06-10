package notification

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// funcDoer is a programmable httpDoer: each request is handled by fn.
// Shared by the FCM and APNs provider tests.
type funcDoer struct {
	fn func(*http.Request) (*http.Response, error)
}

func (f funcDoer) Do(req *http.Request) (*http.Response, error) { return f.fn(req) }

// fakeDeviceTokenRepo is an in-memory DeviceTokenRepository keyed by
// (workspace, user, platform, token).
type fakeDeviceTokenRepo struct {
	mu     sync.Mutex
	tokens map[string]DeviceToken
}

func newFakeDeviceTokenRepo() *fakeDeviceTokenRepo {
	return &fakeDeviceTokenRepo{tokens: map[string]DeviceToken{}}
}

func dtKey(ws, user uuid.UUID, dt DeviceToken) string {
	return ws.String() + "|" + user.String() + "|" + string(dt.Platform) + "|" + dt.Token
}

func (f *fakeDeviceTokenRepo) SaveDeviceToken(_ context.Context, ws, user uuid.UUID, dt DeviceToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[dtKey(ws, user, dt)] = dt
	return nil
}

func (f *fakeDeviceTokenRepo) DeleteDeviceToken(_ context.Context, ws, user uuid.UUID, dt DeviceToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tokens, dtKey(ws, user, dt))
	return nil
}

func (f *fakeDeviceTokenRepo) ListDeviceTokens(_ context.Context, ws, user uuid.UUID) ([]DeviceToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := ws.String() + "|" + user.String() + "|"
	var out []DeviceToken
	for k, dt := range f.tokens {
		if strings.HasPrefix(k, prefix) {
			out = append(out, dt)
		}
	}
	return out, nil
}

func (f *fakeDeviceTokenRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokens)
}

// fakeProvider is a programmable MobilePushProvider. It records the
// tokens it was asked to deliver to and returns a canned (dead, err) per
// call.
type fakeProvider struct {
	platform Platform
	mu       sync.Mutex
	sent     []string
	dead     bool
	err      error
}

func (p *fakeProvider) Platform() Platform { return p.platform }

func (p *fakeProvider) Send(_ context.Context, token string, _ NotificationPayload) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, token)
	return p.dead, p.err
}

func (p *fakeProvider) sentTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sent...)
}

func TestMobilePushServiceRegisterValidation(t *testing.T) {
	ws, user := uuid.New(), uuid.New()
	repo := newFakeDeviceTokenRepo()
	svc := NewMobilePushService(repo).WithProvider(&fakeProvider{platform: PlatformAndroid})

	cases := []struct {
		name    string
		dt      DeviceToken
		wantErr error
	}{
		{"unknown platform", DeviceToken{Platform: "windows", Token: "t"}, ErrInvalidDeviceToken},
		{"empty token", DeviceToken{Platform: PlatformAndroid, Token: ""}, ErrInvalidDeviceToken},
		{"oversized token", DeviceToken{Platform: PlatformAndroid, Token: strings.Repeat("x", maxDeviceTokenLen+1)}, ErrInvalidDeviceToken},
		{"unconfigured platform", DeviceToken{Platform: PlatformIOS, Token: "t"}, ErrPlatformUnsupported},
		{"valid", DeviceToken{Platform: PlatformAndroid, Token: "good-token"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Register(context.Background(), ws, user, tc.dt)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Register: unexpected error %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Register: got %v, want %v", err, tc.wantErr)
			}
		})
	}
	if repo.count() != 1 {
		t.Fatalf("expected exactly the one valid token persisted, got %d", repo.count())
	}
}

func TestMobilePushServiceSendRoutesAndPrunes(t *testing.T) {
	ws, user := uuid.New(), uuid.New()
	repo := newFakeDeviceTokenRepo()
	android := &fakeProvider{platform: PlatformAndroid}
	ios := &fakeProvider{platform: PlatformIOS, dead: true} // iOS token reports dead
	svc := NewMobilePushService(repo).WithProvider(android).WithProvider(ios)

	mustRegister(t, svc, ws, user, DeviceToken{Platform: PlatformAndroid, Token: "android-1"})
	mustRegister(t, svc, ws, user, DeviceToken{Platform: PlatformIOS, Token: "ios-1"})

	if err := svc.Send(context.Background(), ws, user, NotificationPayload{Title: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := android.sentTokens(); len(got) != 1 || got[0] != "android-1" {
		t.Fatalf("android provider got %v, want [android-1]", got)
	}
	if got := ios.sentTokens(); len(got) != 1 || got[0] != "ios-1" {
		t.Fatalf("ios provider got %v, want [ios-1]", got)
	}
	// The iOS token was reported dead → pruned; the Android token remains.
	remaining, _ := repo.ListDeviceTokens(context.Background(), ws, user)
	if len(remaining) != 1 || remaining[0].Token != "android-1" {
		t.Fatalf("after prune got %v, want only android-1", remaining)
	}
}

func TestMobilePushServiceSendTransientDoesNotPrune(t *testing.T) {
	ws, user := uuid.New(), uuid.New()
	repo := newFakeDeviceTokenRepo()
	android := &fakeProvider{platform: PlatformAndroid, err: errors.New("boom")}
	svc := NewMobilePushService(repo).WithProvider(android)
	mustRegister(t, svc, ws, user, DeviceToken{Platform: PlatformAndroid, Token: "android-1"})

	if err := svc.Send(context.Background(), ws, user, NotificationPayload{Title: "hi"}); err != nil {
		t.Fatalf("Send should swallow per-token errors, got %v", err)
	}
	if repo.count() != 1 {
		t.Fatalf("transient error must not prune the token, count=%d", repo.count())
	}
}

func TestMobilePushServiceSkipsUnconfiguredPlatform(t *testing.T) {
	ws, user := uuid.New(), uuid.New()
	repo := newFakeDeviceTokenRepo()
	// Persist an iOS token directly (bypassing Register's provider check) to
	// simulate a provider that was later unconfigured.
	_ = repo.SaveDeviceToken(context.Background(), ws, user, DeviceToken{Platform: PlatformIOS, Token: "ios-orphan"})
	android := &fakeProvider{platform: PlatformAndroid}
	svc := NewMobilePushService(repo).WithProvider(android)

	if err := svc.Send(context.Background(), ws, user, NotificationPayload{Title: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(android.sentTokens()) != 0 {
		t.Fatalf("android provider should not receive an iOS token")
	}
	// Orphan token left in place (provider may be reconfigured later).
	if repo.count() != 1 {
		t.Fatalf("orphan token must not be pruned, count=%d", repo.count())
	}
}

func TestMobilePushServiceDisabledNoProviders(t *testing.T) {
	svc := NewMobilePushService(newFakeDeviceTokenRepo())
	if svc.Enabled() {
		t.Fatal("service with no providers must be disabled")
	}
	var nilSvc *MobilePushService
	if nilSvc.Enabled() {
		t.Fatal("nil service must be disabled")
	}
	// nil-safe Send.
	if err := nilSvc.Send(context.Background(), uuid.New(), uuid.New(), NotificationPayload{}); err != nil {
		t.Fatalf("nil Send: %v", err)
	}
}

func TestMobilePushPublisherFansOutNotification(t *testing.T) {
	inner := &recordingPublisher{}
	push := &recordingMobileSender{}
	var wg sync.WaitGroup
	pub := NewMobilePushPublisher(inner, push).WithWaitGroup(&wg)

	ws, user := uuid.New(), uuid.New()
	n := &Notification{ID: uuid.New(), Title: "t", Body: "b", Type: "notification"}
	if err := pub.Publish(context.Background(), ws, user, Event{Type: "notification", Payload: n}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	wg.Wait() // drain the detached push goroutine deterministically

	if inner.calls != 1 {
		t.Fatalf("inner publisher calls=%d, want 1", inner.calls)
	}
	if push.calls != 1 {
		t.Fatalf("mobile sender calls=%d, want 1", push.calls)
	}
}

func TestMobilePushPublisherIgnoresNonNotification(t *testing.T) {
	inner := &recordingPublisher{}
	push := &recordingMobileSender{}
	pub := NewMobilePushPublisher(inner, push)
	if err := pub.Publish(context.Background(), uuid.New(), uuid.New(), Event{Type: "presence", Payload: nil}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if push.calls != 0 {
		t.Fatalf("mobile sender must not fire for non-notification events, calls=%d", push.calls)
	}
}

func TestMobilePushPublisherReturnsInnerError(t *testing.T) {
	wantErr := errors.New("inner boom")
	inner := &errPublisher{err: wantErr}
	pub := NewMobilePushPublisher(inner, &recordingMobileSender{})
	err := pub.Publish(context.Background(), uuid.New(), uuid.New(), Event{Type: "presence"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Publish err=%v, want inner error", err)
	}
}

// errPublisher is a WSPublisher that always returns a canned error, used
// to assert the decorator surfaces the inner publish result.
type errPublisher struct{ err error }

func (p *errPublisher) Publish(_ context.Context, _, _ uuid.UUID, _ Event) error {
	return p.err
}

func mustRegister(t *testing.T, svc *MobilePushService, ws, user uuid.UUID, dt DeviceToken) {
	t.Helper()
	if err := svc.Register(context.Background(), ws, user, dt); err != nil {
		t.Fatalf("Register(%s): %v", dt.Token, err)
	}
}

// recordingMobileSender is a MobilePushSender that counts calls.
type recordingMobileSender struct {
	mu    sync.Mutex
	calls int
}

func (r *recordingMobileSender) Send(_ context.Context, _, _ uuid.UUID, _ NotificationPayload) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return nil
}
