package notification

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// testP8Key returns a PEM-encoded PKCS#8 EC P-256 private key, matching
// the format Apple issues .p8 auth keys in.
func testP8Key(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func newTestAPNs(t *testing.T, stub httpDoer) *APNsProvider {
	t.Helper()
	p, err := NewAPNsProvider(testP8Key(t), "KEY123456", "TEAM123456", "com.uney.zkdrive", false)
	if err != nil {
		t.Fatalf("NewAPNsProvider: %v", err)
	}
	return p.WithHTTPClient(stub)
}

// apnsStub records calls and returns a canned status + reason body.
type apnsStub struct {
	mu       sync.Mutex
	calls    int
	status   int
	reason   string
	lastPath string
	lastAuth string
	lastTop  string
}

func (s *apnsStub) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastPath = req.URL.Path
	s.lastAuth = req.Header.Get("authorization")
	s.lastTop = req.Header.Get("apns-topic")
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	body := ""
	if s.reason != "" {
		body = `{"reason":"` + s.reason + `"}`
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func TestAPNsSendDeliversWithHeaders(t *testing.T) {
	stub := &apnsStub{}
	p := newTestAPNs(t, stub)
	dead, err := p.Send(context.Background(), "abc123", NotificationPayload{Title: "t", Body: "b"})
	if err != nil || dead {
		t.Fatalf("send: dead=%v err=%v, want delivered", dead, err)
	}
	if stub.lastPath != "/3/device/abc123" {
		t.Fatalf("path = %q, want /3/device/abc123", stub.lastPath)
	}
	if !strings.HasPrefix(stub.lastAuth, "bearer ") {
		t.Fatalf("authorization = %q, want bearer token", stub.lastAuth)
	}
	if stub.lastTop != "com.uney.zkdrive" {
		t.Fatalf("apns-topic = %q", stub.lastTop)
	}
}

func TestAPNsTokenIsReused(t *testing.T) {
	stub := &apnsStub{}
	p := newTestAPNs(t, stub)
	base := time.Now()
	p.now = func() time.Time { return base }
	if _, err := p.Send(context.Background(), "a", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	firstAuth := stub.lastAuth
	// Within apnsTokenTTL: same token reused.
	p.now = func() time.Time { return base.Add(apnsTokenTTL - time.Minute) }
	if _, err := p.Send(context.Background(), "b", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if stub.lastAuth != firstAuth {
		t.Fatalf("provider token should be reused within TTL")
	}
	// Past TTL: re-signed (iat changes → different JWT).
	p.now = func() time.Time { return base.Add(apnsTokenTTL + time.Minute) }
	if _, err := p.Send(context.Background(), "c", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if stub.lastAuth == firstAuth {
		t.Fatalf("provider token should be re-signed past TTL")
	}
}

func TestAPNsDeadTokenStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reason string
	}{
		{"410 unregistered", http.StatusGone, "Unregistered"},
		{"400 bad device token", http.StatusBadRequest, "BadDeviceToken"},
		{"400 not for topic", http.StatusBadRequest, "DeviceTokenNotForTopic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &apnsStub{status: tc.status, reason: tc.reason}
			p := newTestAPNs(t, stub)
			dead, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !dead {
				t.Fatalf("%s must be reported dead", tc.name)
			}
		})
	}
}

func TestAPNsExpiredProviderTokenIsTransient(t *testing.T) {
	stub := &apnsStub{status: http.StatusForbidden, reason: "ExpiredProviderToken"}
	p := newTestAPNs(t, stub)
	base := time.Now()
	p.now = func() time.Time { return base }
	dead, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"})
	if dead {
		t.Fatal("ExpiredProviderToken must not prune the device token")
	}
	if err == nil {
		t.Fatal("ExpiredProviderToken must surface a transient error")
	}
	// Cached token was invalidated → next send re-signs (even within TTL).
	firstAuth := stub.lastAuth
	stub.status = http.StatusOK
	stub.reason = ""
	if _, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("recovery send: %v", err)
	}
	if stub.lastAuth == firstAuth {
		t.Fatal("token should be re-signed after ExpiredProviderToken")
	}
}

func TestAPNsTransientReason(t *testing.T) {
	stub := &apnsStub{status: http.StatusServiceUnavailable, reason: "ServiceUnavailable"}
	p := newTestAPNs(t, stub)
	dead, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"})
	if dead {
		t.Fatal("503 must not prune the token")
	}
	if err == nil {
		t.Fatal("503 must surface a transient error")
	}
}

func TestAPNsPayloadShape(t *testing.T) {
	var captured []byte
	stub := funcDoer{fn: func(req *http.Request) (*http.Response, error) {
		captured, _ = io.ReadAll(req.Body)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	}}
	p := newTestAPNs(t, stub)
	if _, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "Hello", Body: "World", URL: "/drive/folder/1", Type: "notification", Tag: "n1"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(captured, &doc); err != nil {
		t.Fatalf("payload not valid json: %v", err)
	}
	aps, ok := doc["aps"].(map[string]any)
	if !ok {
		t.Fatalf("missing aps dict: %v", doc)
	}
	alert, ok := aps["alert"].(map[string]any)
	if !ok {
		t.Fatalf("missing alert dict: %v", aps)
	}
	if alert["title"] != "Hello" || alert["body"] != "World" {
		t.Fatalf("alert = %v", alert)
	}
	if doc["url"] != "/drive/folder/1" || doc["type"] != "notification" {
		t.Fatalf("custom keys = %v", doc)
	}
}

func TestAPNsRejectsBadConfig(t *testing.T) {
	if _, err := NewAPNsProvider(testP8Key(t), "", "team", "topic", false); err == nil {
		t.Fatal("expected error for missing key id")
	}
	if _, err := NewAPNsProvider([]byte("not pem"), "kid", "team", "topic", false); err == nil {
		t.Fatal("expected error for invalid key")
	}
}
