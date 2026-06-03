package middleware

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// fakeIPChecker records the IP it was asked about and returns a
// canned result, so the middleware's IP extraction and error mapping
// can be tested in isolation from the real service.
type fakeIPChecker struct {
	err   error
	gotIP net.IP
	gotWS uuid.UUID
	calls int
}

func (f *fakeIPChecker) CheckAccess(_ context.Context, workspaceID uuid.UUID, clientIP net.IP) error {
	f.calls++
	f.gotWS = workspaceID
	f.gotIP = clientIP
	return f.err
}

func newAuthedRequest(ws uuid.UUID, remoteAddr, xff string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	ctx := WithWorkspaceID(req.Context(), ws)
	return req.WithContext(ctx)
}

func TestIPAllowlist_NilCheckerPassesThrough(t *testing.T) {
	var called bool
	h := IPAllowlist(nil, 1)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !called {
		t.Fatalf("nil checker should be a pass-through")
	}
}

// TestIPAllowlist_TypedNilCheckerPassesThrough guards the typed-nil
// case: a nil *workspace.IPAllowService passed through the interface
// is not == nil, so without the unwrap it would panic in CheckAccess.
// It must instead disable enforcement, like an untyped nil.
func TestIPAllowlist_TypedNilCheckerPassesThrough(t *testing.T) {
	var svc *workspace.IPAllowService // typed nil
	var called bool
	h := IPAllowlist(svc, 1)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAuthedRequest(uuid.New(), "70.0.0.1:1234", ""))
	if !called {
		t.Fatalf("typed-nil checker should be a pass-through")
	}
}

func TestIPAllowlist_AllowedCallsNext(t *testing.T) {
	ws := uuid.New()
	checker := &fakeIPChecker{err: nil}
	var called bool
	h := IPAllowlist(checker, 1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAuthedRequest(ws, "70.0.0.1:1234", ""))

	if !called {
		t.Fatalf("allowed request should reach next handler")
	}
	if checker.gotWS != ws {
		t.Fatalf("workspace not threaded: got %s want %s", checker.gotWS, ws)
	}
}

func TestIPAllowlist_BlockedReturns403WithHeader(t *testing.T) {
	ws := uuid.New()
	checker := &fakeIPChecker{err: workspace.ErrIPBlocked}
	var called bool
	h := IPAllowlist(checker, 1)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAuthedRequest(ws, "70.0.0.1:1234", ""))

	if called {
		t.Fatalf("blocked request must not reach next handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get(IPBlockedHeader); got != "true" {
		t.Fatalf("%s header: got %q want %q", IPBlockedHeader, got, "true")
	}
}

func TestIPAllowlist_ServiceErrorFailsClosed(t *testing.T) {
	ws := uuid.New()
	checker := &fakeIPChecker{err: context.DeadlineExceeded}
	var called bool
	h := IPAllowlist(checker, 1)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAuthedRequest(ws, "70.0.0.1:1234", ""))

	if called {
		t.Fatalf("a service error must not admit the request")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestIPAllowlist_MissingWorkspaceFailsClosed(t *testing.T) {
	checker := &fakeIPChecker{}
	h := IPAllowlist(checker, 1)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	// No workspace bound in context.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/files", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusInternalServerError)
	}
	if checker.calls != 0 {
		t.Fatalf("checker must not be consulted without a workspace")
	}
}

func TestClientIPFromRequest_TrustedProxyDepth(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		depth      int
		want       string
	}{
		{
			name:       "depth1 single proxy uses last xff entry",
			remoteAddr: "10.0.0.1:5000",
			xff:        "203.0.113.9",
			depth:      1,
			want:       "203.0.113.9",
		},
		{
			name:       "depth1 ignores spoofed leading entries",
			remoteAddr: "10.0.0.1:5000",
			xff:        "1.1.1.1, 2.2.2.2, 203.0.113.9",
			depth:      1,
			want:       "203.0.113.9",
		},
		{
			name:       "depth2 takes second-from-right",
			remoteAddr: "10.0.0.1:5000",
			xff:        "203.0.113.9, 198.51.100.7",
			depth:      2,
			want:       "203.0.113.9",
		},
		{
			name:       "depth exceeds entries falls back to remoteaddr",
			remoteAddr: "70.0.0.5:5000",
			xff:        "203.0.113.9",
			depth:      3,
			want:       "70.0.0.5",
		},
		{
			name:       "no xff uses remoteaddr",
			remoteAddr: "70.0.0.5:5000",
			xff:        "",
			depth:      1,
			want:       "70.0.0.5",
		},
		{
			name:       "depth zero ignores xff and uses remoteaddr",
			remoteAddr: "70.0.0.5:5000",
			xff:        "203.0.113.9",
			depth:      0,
			want:       "70.0.0.5",
		},
		{
			name:       "ipv6 bracketed remoteaddr",
			remoteAddr: "[2001:db8::1]:5000",
			xff:        "",
			depth:      1,
			want:       "2001:db8::1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := ClientIPFromRequest(req, tc.depth)
			want := net.ParseIP(tc.want)
			if !got.Equal(want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// TestIPAllowlist_ExtractsTrustedIP wires the extraction into the
// middleware end-to-end: the checker should receive the trusted XFF
// entry, not the spoofed leading one.
func TestIPAllowlist_ExtractsTrustedIP(t *testing.T) {
	ws := uuid.New()
	checker := &fakeIPChecker{}
	h := IPAllowlist(checker, 1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newAuthedRequest(ws, "10.0.0.1:5000", "9.9.9.9, 203.0.113.9"))

	if !checker.gotIP.Equal(net.ParseIP("203.0.113.9")) {
		t.Fatalf("checker saw %v, want trusted entry 203.0.113.9", checker.gotIP)
	}
}
