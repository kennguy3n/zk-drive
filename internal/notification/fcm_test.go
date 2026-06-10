package notification

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
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

// testServiceAccountJSON builds a syntactically valid service-account
// JSON with a freshly generated RSA key so the provider can actually
// sign the assertion. token_uri points at a sentinel host the stub
// routes on.
func testServiceAccountJSON(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa := map[string]string{
		"type":         "service_account",
		"project_id":   "test-project",
		"private_key":  string(pemBytes),
		"client_email": "fcm@test-project.iam.gserviceaccount.com",
		"token_uri":    "https://token.test/token",
	}
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatalf("marshal sa: %v", err)
	}
	return raw
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// fcmStub routes token-endpoint vs send-endpoint requests and counts each.
type fcmStub struct {
	mu          sync.Mutex
	tokenCalls  int
	sendCalls   int
	sendStatus  int
	sendBody    string
	lastAuth    string
	accessToken string
}

func (s *fcmStub) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Contains(req.URL.String(), "token.test") {
		s.tokenCalls++
		at := s.accessToken
		if at == "" {
			at = "access-token-1"
		}
		return jsonResp(http.StatusOK, `{"access_token":"`+at+`","expires_in":3600}`), nil
	}
	// send endpoint
	s.sendCalls++
	s.lastAuth = req.Header.Get("Authorization")
	status := s.sendStatus
	if status == 0 {
		status = http.StatusOK
	}
	return jsonResp(status, s.sendBody), nil
}

func newTestFCM(t *testing.T, stub httpDoer) *FCMProvider {
	t.Helper()
	p, err := NewFCMProvider(testServiceAccountJSON(t))
	if err != nil {
		t.Fatalf("NewFCMProvider: %v", err)
	}
	return p.WithHTTPClient(stub)
}

func TestFCMSendDeliversAndCachesToken(t *testing.T) {
	stub := &fcmStub{}
	p := newTestFCM(t, stub)

	dead, err := p.Send(context.Background(), "device-token", NotificationPayload{Title: "t", Body: "b", Tag: "tag1"})
	if err != nil || dead {
		t.Fatalf("first send: dead=%v err=%v, want delivered", dead, err)
	}
	if stub.lastAuth != "Bearer access-token-1" {
		t.Fatalf("Authorization header = %q, want bearer access token", stub.lastAuth)
	}
	// Second send reuses the cached access token: no second token exchange.
	if _, err := p.Send(context.Background(), "device-token", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if stub.tokenCalls != 1 {
		t.Fatalf("token exchanges = %d, want 1 (cached)", stub.tokenCalls)
	}
	if stub.sendCalls != 2 {
		t.Fatalf("send calls = %d, want 2", stub.sendCalls)
	}
}

func TestFCMSendUnregisteredIsDead(t *testing.T) {
	stub := &fcmStub{
		sendStatus: http.StatusNotFound,
		sendBody:   `{"error":{"code":404,"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`,
	}
	p := newTestFCM(t, stub)
	dead, err := p.Send(context.Background(), "stale", NotificationPayload{Title: "t"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !dead {
		t.Fatal("UNREGISTERED token must be reported dead")
	}
}

func TestFCMSendSenderIDMismatchIsDead(t *testing.T) {
	stub := &fcmStub{
		sendStatus: http.StatusForbidden,
		sendBody:   `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`,
	}
	p := newTestFCM(t, stub)
	dead, err := p.Send(context.Background(), "wrong-sender", NotificationPayload{Title: "t"})
	if err != nil || !dead {
		t.Fatalf("SENDER_ID_MISMATCH: dead=%v err=%v, want dead", dead, err)
	}
}

func TestFCMSendUnauthorizedInvalidatesTokenAndIsTransient(t *testing.T) {
	stub := &fcmStub{
		sendStatus: http.StatusUnauthorized,
		sendBody:   `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`,
	}
	p := newTestFCM(t, stub)
	dead, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"})
	if dead {
		t.Fatal("401 must not prune the device token")
	}
	if err == nil {
		t.Fatal("401 must surface a transient error")
	}
	// Token cache was invalidated → next send re-mints.
	stub.sendStatus = http.StatusOK
	stub.sendBody = ""
	if _, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("recovery send: %v", err)
	}
	if stub.tokenCalls != 2 {
		t.Fatalf("token exchanges = %d, want 2 (re-mint after 401)", stub.tokenCalls)
	}
}

func TestFCMSendInvalidArgumentIsTransientNotDead(t *testing.T) {
	stub := &fcmStub{
		sendStatus: http.StatusBadRequest,
		sendBody:   `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"errorCode":"INVALID_ARGUMENT"}]}}`,
	}
	p := newTestFCM(t, stub)
	dead, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"})
	if dead {
		t.Fatal("INVALID_ARGUMENT may be a payload bug, not a dead token; must not prune")
	}
	if err == nil {
		t.Fatal("INVALID_ARGUMENT must surface a transient error")
	}
}

func TestFCMTokenRefreshesBeforeExpiry(t *testing.T) {
	stub := &fcmStub{}
	p := newTestFCM(t, stub)
	base := time.Now()
	p.now = func() time.Time { return base }

	if _, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Advance to within the refresh skew of the 3600s token: must re-mint.
	p.now = func() time.Time { return base.Add(3600*time.Second - 30*time.Second) }
	if _, err := p.Send(context.Background(), "tok", NotificationPayload{Title: "t"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if stub.tokenCalls != 2 {
		t.Fatalf("token exchanges = %d, want 2 (refresh within skew)", stub.tokenCalls)
	}
}

func TestFCMRejectsBadServiceAccount(t *testing.T) {
	if _, err := NewFCMProvider([]byte(`{"project_id":"p"}`)); err == nil {
		t.Fatal("expected error for service account missing client_email/private_key")
	}
	if _, err := NewFCMProvider([]byte(`not json`)); err == nil {
		t.Fatal("expected error for malformed json")
	}
}

// ensure the request body is valid JSON carrying the token & notification.
func TestFCMSendRequestBody(t *testing.T) {
	var captured []byte
	stub := funcDoer{fn: func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.String(), "token.test") {
			return jsonResp(http.StatusOK, `{"access_token":"at","expires_in":3600}`), nil
		}
		captured, _ = io.ReadAll(req.Body)
		return jsonResp(http.StatusOK, ""), nil
	}}
	p := newTestFCM(t, stub)
	if _, err := p.Send(context.Background(), "dev-tok", NotificationPayload{Title: "Hello", Body: "World", Type: "notification", URL: "/drive", Tag: "n1"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var sent fcmSendRequest
	if err := json.NewDecoder(bytes.NewReader(captured)).Decode(&sent); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	if sent.Message.Token != "dev-tok" {
		t.Fatalf("token = %q", sent.Message.Token)
	}
	if sent.Message.Notification.Title != "Hello" || sent.Message.Notification.Body != "World" {
		t.Fatalf("notification = %+v", sent.Message.Notification)
	}
	if sent.Message.Data["url"] != "/drive" || sent.Message.Data["type"] != "notification" {
		t.Fatalf("data = %+v", sent.Message.Data)
	}
}
