package webhooks

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// loopbackValidator returns a URLValidator wired with a fake
// resolver that resolves any hostname to the test server's IP. The
// production validator would block 127.0.0.1 — but tests need to
// hit httptest.NewServer (which binds to loopback) so we set
// AllowLoopback explicitly.
func loopbackValidator(t *testing.T, server *httptest.Server) *URLValidator {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	host, _, _ := net.SplitHostPort(u.Host)
	if host == "" {
		host = u.Host
	}
	v := NewURLValidator()
	v.AllowHTTP = true
	v.AllowLoopback = true
	v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
		host: {{IP: net.ParseIP(host)}},
	}}
	return v
}

func TestDeliveryClient_Deliver_Success(t *testing.T) {
	t.Parallel()
	var capturedBody string
	var capturedHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		capturedBody = string(buf[:n])
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	v := loopbackValidator(t, srv)
	c := NewDeliveryClient(v, 5*time.Second)
	signer, _ := NewSigner(secret32)
	u, _ := url.Parse(srv.URL)
	body := []byte(`{"hello":"world"}`)
	a := c.Deliver(context.Background(), u, uuid.New(), uuid.New(), EventFileUploadConfirmed, body, signer, time.Unix(1_700_000_000, 0))

	if a.Outcome != OutcomeSuccess {
		t.Fatalf("outcome: got=%s want=%s err=%q", a.Outcome, OutcomeSuccess, a.ErrorMessage)
	}
	if a.StatusCode != http.StatusNoContent {
		t.Errorf("status: got=%d want=%d", a.StatusCode, http.StatusNoContent)
	}
	if capturedBody != string(body) {
		t.Errorf("body: got=%q want=%q", capturedBody, body)
	}
	if !strings.HasPrefix(capturedHeaders.Get(SignatureHeader), "t=1700000000,v1=") {
		t.Errorf("signature header missing/malformed: %q", capturedHeaders.Get(SignatureHeader))
	}
	if capturedHeaders.Get(EventTypeHeader) != string(EventFileUploadConfirmed) {
		t.Errorf("event-type header: %q", capturedHeaders.Get(EventTypeHeader))
	}
	if capturedHeaders.Get(EventIDHeader) == "" {
		t.Errorf("event-id header missing")
	}
	if capturedHeaders.Get(DeliveryIDHeader) == "" {
		t.Errorf("delivery-id header missing")
	}
}

func TestDeliveryClient_Deliver_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()
	v := loopbackValidator(t, srv)
	c := NewDeliveryClient(v, 5*time.Second)
	signer, _ := NewSigner(secret32)
	u, _ := url.Parse(srv.URL)
	a := c.Deliver(context.Background(), u, uuid.New(), uuid.New(), EventFileDeleted, []byte("x"), signer, time.Now())
	if a.Outcome != OutcomeHTTPError {
		t.Fatalf("outcome: got=%s want=%s", a.Outcome, OutcomeHTTPError)
	}
	if a.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: got=%d", a.StatusCode)
	}
	if a.ResponseBody != "upstream down" {
		t.Errorf("response body: got=%q", a.ResponseBody)
	}
}

func TestDeliveryClient_Deliver_NetError(t *testing.T) {
	t.Parallel()
	// Spin up a server then close it so the connection is refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()
	u, _ := url.Parse(addr)
	host, _, _ := net.SplitHostPort(u.Host)
	if host == "" {
		host = u.Host
	}
	v := NewURLValidator()
	v.AllowHTTP = true
	v.AllowLoopback = true
	v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
		host: {{IP: net.ParseIP(host)}},
	}}
	c := NewDeliveryClient(v, 500*time.Millisecond)
	signer, _ := NewSigner(secret32)
	a := c.Deliver(context.Background(), u, uuid.New(), uuid.New(), EventFileDeleted, []byte("x"), signer, time.Now())
	if a.Outcome != OutcomeNetError {
		t.Fatalf("outcome: got=%s want=%s err=%q", a.Outcome, OutcomeNetError, a.ErrorMessage)
	}
}

func TestDeliveryClient_Deliver_BlockedByRebinding(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// At "create-time" the validator passes the URL — but for the
	// delivery attempt we swap the resolver to a blocked address so
	// ValidateResolved fails. The delivery client must abandon the
	// request and record outcome=blocked.
	v := NewURLValidator()
	v.AllowHTTP = true
	v.Resolver = &fakeResolver{hosts: map[string][]net.IPAddr{
		"victim.example.com": {{IP: net.ParseIP("169.254.169.254")}},
	}}
	c := NewDeliveryClient(v, 5*time.Second)
	signer, _ := NewSigner(secret32)
	u, _ := url.Parse("http://victim.example.com/path")
	a := c.Deliver(context.Background(), u, uuid.New(), uuid.New(), EventFileDeleted, []byte("x"), signer, time.Now())
	if a.Outcome != OutcomeBlocked {
		t.Fatalf("outcome: got=%s want=%s err=%q", a.Outcome, OutcomeBlocked, a.ErrorMessage)
	}
	if a.StatusCode != 0 {
		t.Errorf("status code on blocked: got=%d want=0", a.StatusCode)
	}
}

func TestDeliveryClient_Deliver_TruncatesLargeResponse(t *testing.T) {
	t.Parallel()
	// Server responds with > DefaultMaxResponseBodyBytes of body.
	// Client must truncate and tag with "[truncated]".
	body := strings.Repeat("A", int(DefaultMaxResponseBodyBytes)+100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	v := loopbackValidator(t, srv)
	c := NewDeliveryClient(v, 5*time.Second)
	signer, _ := NewSigner(secret32)
	u, _ := url.Parse(srv.URL)
	a := c.Deliver(context.Background(), u, uuid.New(), uuid.New(), EventFileDeleted, []byte("x"), signer, time.Now())
	if a.Outcome != OutcomeSuccess {
		t.Fatalf("outcome: got=%s err=%q", a.Outcome, a.ErrorMessage)
	}
	if !strings.HasSuffix(a.ResponseBody, " [truncated]") {
		t.Errorf("response body not tagged truncated: tail=%q", a.ResponseBody[len(a.ResponseBody)-20:])
	}
	if int64(len(a.ResponseBody)) > DefaultMaxResponseBodyBytes+int64(len(" [truncated]")) {
		t.Errorf("response body too large: len=%d", len(a.ResponseBody))
	}
}

func TestDeliveryClient_Deliver_RejectsRedirects(t *testing.T) {
	t.Parallel()
	// A 302 redirect must be treated as an HTTP error (no
	// follow-the-redirect), so a malicious endpoint can't bounce
	// us to a private IP.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://attacker.example.com/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	v := loopbackValidator(t, srv)
	c := NewDeliveryClient(v, 5*time.Second)
	signer, _ := NewSigner(secret32)
	u, _ := url.Parse(srv.URL)
	a := c.Deliver(context.Background(), u, uuid.New(), uuid.New(), EventFileDeleted, []byte("x"), signer, time.Now())
	if a.Outcome != OutcomeHTTPError {
		t.Fatalf("outcome: got=%s want=%s err=%q", a.Outcome, OutcomeHTTPError, a.ErrorMessage)
	}
	if a.StatusCode != http.StatusFound {
		t.Errorf("status: got=%d want=302", a.StatusCode)
	}
}

func TestBackoffDelay_Schedule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 0},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 8 * time.Second},
	}
	for _, c := range cases {
		c := c
		if got := BackoffDelay(c.attempt); got != c.want {
			t.Errorf("BackoffDelay(%d)=%s want=%s", c.attempt, got, c.want)
		}
	}
}
