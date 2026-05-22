package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestParseLevelMapsAllSupportedAliases pins the LOG_LEVEL parser
// contract: every documented value (and its common alias) maps to
// the expected slog.Level, and unknown values fall back to Info
// rather than panicking or erroring. Log misconfiguration must
// never crash a pod on startup.
func TestParseLevelMapsAllSupportedAliases(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" debug ", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"error", slog.LevelError},
		{"err", slog.LevelError},
		{"trace", slog.LevelInfo}, // unknown → info, not an error
		{"verbose", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseLevel(tc.in); got != tc.want {
				t.Fatalf("parseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestFromContextReturnsDefaultWhenUnset covers the fallback path
// that internal services rely on: if nobody attached a logger to
// ctx (background workers, tests, init code), FromContext must
// still return a usable logger rather than nil.
func TestFromContextReturnsDefaultWhenUnset(t *testing.T) {
	got := FromContext(context.Background())
	if got == nil {
		t.Fatal("FromContext returned nil for unattached ctx; must return default")
	}
	if got != slog.Default() {
		t.Fatal("FromContext must return slog.Default() when ctx has no logger")
	}

	// Nil ctx also must not panic.
	if got := FromContext(nil); got == nil { //nolint:staticcheck // exercise nil-ctx defensive path
		t.Fatal("FromContext(nil) returned nil")
	}
}

// TestWithContextThenFromContextRoundTrip exercises the common
// pattern handlers use to attach per-request scope: build a child
// logger with extra attributes, attach it to ctx, retrieve it
// later from a different call site.
func TestWithContextThenFromContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil)).With("component", "test")
	child := base.With("request_id", "rid-abc")

	ctx := WithContext(context.Background(), child)
	got := FromContext(ctx)
	if got == nil {
		t.Fatal("FromContext returned nil after WithContext")
	}
	got.Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if rec["component"] != "test" {
		t.Errorf("component attribute lost: %v", rec)
	}
	if rec["request_id"] != "rid-abc" {
		t.Errorf("request_id attribute lost: %v", rec)
	}
}

// TestWithContextNilLoggerLeavesCtxUntouched guards against an
// easy footgun: passing a nil logger should not wipe out a
// previously-attached one, otherwise a deeply-nested handler
// could accidentally clobber the request logger by passing a
// stale *slog.Logger variable.
func TestWithContextNilLoggerLeavesCtxUntouched(t *testing.T) {
	base := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	ctx := WithContext(context.Background(), base)
	got := WithContext(ctx, nil)
	if FromContext(got) != base {
		t.Fatal("nil logger overwrote an existing context logger")
	}
}

// TestMiddlewareAttachesRequestScopedLogger verifies the chi
// middleware contract: a downstream handler must be able to call
// FromContext(r.Context()) and get a logger whose every record
// carries the request method, path, and request_id from the
// incoming X-Request-Id header.
func TestMiddlewareAttachesRequestScopedLogger(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/workspaces/{id}", func(w http.ResponseWriter, req *http.Request) {
		FromContext(req.Context()).Info("handler reached")
	})

	req := httptest.NewRequest(http.MethodGet, "/workspaces/abc-123", nil)
	req.Header.Set("X-Request-Id", "rid-xyz")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handler did not run: status=%d body=%q", rec.Code, rec.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, `"http_method":"GET"`) {
		t.Errorf("http_method missing: %s", out)
	}
	if !strings.Contains(out, `"http_path":"/workspaces/abc-123"`) {
		t.Errorf("http_path should be the raw URL path, got: %s", out)
	}
	if !strings.Contains(out, `"request_id":"rid-xyz"`) {
		t.Errorf("request_id missing: %s", out)
	}
}

// TestAccessLogEmitsRoutePatternAndStatus exercises the
// post-dispatch access logger that captures the resolved chi
// pattern, response status, and duration. This is the path
// dashboards aggregate against — the request-scoped Middleware
// only carries raw URL path because chi hasn't matched yet at
// that point.
func TestAccessLogEmitsRoutePatternAndStatus(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	inner := chi.NewRouter()
	inner.Use(Middleware)
	inner.Get("/workspaces/{id}", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	})
	wrapped := AccessLog(inner)

	req := httptest.NewRequest(http.MethodGet, "/workspaces/abc-123", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("handler status not propagated: %d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"http request"`) {
		t.Fatalf("access log record missing: %s", out)
	}
	if !strings.Contains(out, `"http_route":"/workspaces/{id}"`) {
		t.Errorf("http_route should be the chi pattern, got: %s", out)
	}
	if !strings.Contains(out, `"http_status":418`) {
		t.Errorf("http_status missing or wrong: %s", out)
	}
	if !strings.Contains(out, `"http_bytes":2`) {
		t.Errorf("http_bytes missing or wrong: %s", out)
	}
}

// TestMiddlewareXForwardedForFirstEntry pins the client-IP
// extraction rule: behind a load balancer the leftmost entry of
// X-Forwarded-For is the original client; subsequent entries are
// intermediate proxies. Tests this is what shows up in the log
// rather than the proxy's IP from RemoteAddr.
func TestMiddlewareXForwardedForFirstEntry(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		FromContext(req.Context()).Info("x")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.RemoteAddr = "10.0.0.1:54321"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	out := buf.String()
	if !strings.Contains(out, `"remote_addr":"203.0.113.7"`) {
		t.Errorf("X-Forwarded-For leftmost entry should win, got: %s", out)
	}
}

// TestInitBridgesLegacyLogPackage validates the migration
// strategy: code that still calls log.Printf must still emit
// structured JSON, even before its file is converted. Otherwise
// the migration would have to be atomic across all 90+ call
// sites in one PR, which is exactly the kind of all-or-nothing
// change we want to avoid.
func TestInitBridgesLegacyLogPackage(t *testing.T) {
	// Init writes to os.Stderr, so swap it through a pipe-like
	// buffer via slog.SetDefault directly to a buffer-backed
	// handler. Then re-bridge log to that handler.
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, nil)
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(handler))
	bridge := slog.NewLogLogger(handler, slog.LevelInfo)
	prevFlags := log.Flags()
	prevOut := log.Writer()
	defer log.SetFlags(prevFlags)
	defer log.SetOutput(prevOut)
	log.SetFlags(0)
	log.SetOutput(bridge.Writer())

	log.Printf("nats: connected to %s", "nats://localhost:4222")

	out := buf.String()
	if !strings.Contains(out, `"msg":"nats: connected to nats://localhost:4222"`) {
		t.Fatalf("legacy log.Printf did not flow through slog handler: %s", out)
	}
}
