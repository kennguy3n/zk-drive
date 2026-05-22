// Package logging configures the process-wide structured logger
// and provides request-scoped helpers for HTTP handlers and
// background workers.
//
// Why slog (and not zap/zerolog/logrus)
//
// Standard library log/slog has been the recommended Go structured
// logger since 1.21. It satisfies the three properties we need:
//
//   - Zero third-party dependencies. The runtime, CI image, and
//     SBOM all stay smaller.
//   - JSON output by default so K8s log shippers (Fluent Bit,
//     Vector, CloudWatch Agent) can index every field without a
//     parser-side regex.
//   - Bridges the legacy `log` package via slog.NewLogLogger and
//     log.SetOutput so any straggler `log.Print*` call routes
//     through the same handler. This lets us migrate
//     incrementally without losing any log lines.
//
// Configuration
//
// Two environment variables tune the handler at process start:
//
//   - LOG_LEVEL: debug | info | warn | error (default: info).
//     Compared case-insensitively. Unknown values fall back to
//     info with a one-time warning rather than crashing — log
//     misconfiguration shouldn't be a SIGTERM.
//   - LOG_FORMAT: json | text (default: json). Text is useful for
//     local dev tail-following; production should always be json.
//
// Request-scoped logging
//
// HTTP handlers should call logging.FromContext(r.Context()) to
// pick up a logger that already has request_id / workspace_id /
// user_id attached. The chi middleware attaches the child logger
// before any application code runs, so a handler that wants to
// log "upload failed" just calls
// logging.FromContext(ctx).Error("upload failed", "err", err) and
// the request correlation falls out of the context for free.
package logging

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// timeNow / timeSince are package vars so tests can pin the
// monotonic clock for deterministic duration assertions.
var (
	timeNow   = time.Now
	timeSince = time.Since
)

// ctxKey is a package-private type so external packages can't
// accidentally collide with our context key by using the same
// string. Standard pattern from net/http.
type ctxKey int

const loggerCtxKey ctxKey = 0

// Init configures the process-wide default logger and returns it
// for callers that want to hold a reference rather than reach for
// slog.Default(). It also bridges the legacy `log` package output
// to the same handler so any un-migrated `log.Printf` calls still
// emit structured JSON. Safe to call exactly once at process
// startup; subsequent calls are a no-op aside from re-bridging.
//
// `component` becomes an attribute on every log line emitted by
// the returned logger (and slog.Default), so a multi-binary
// deployment can distinguish server vs worker vs reconciler
// without parsing the message string.
func Init(component string) *slog.Logger {
	level := parseLevel(os.Getenv("LOG_LEVEL"))
	handler := newHandler(os.Stderr, level)
	logger := slog.New(handler).With("component", component)
	slog.SetDefault(logger)

	// Bridge the legacy `log` package output through slog so a
	// `log.Printf("nats: ...")` call emits the same JSON
	// structure as a native `slog.Info("nats: ...")` call. The
	// LstdFlags reset prevents log from prefixing the message
	// with a date-time that the JSON handler would duplicate.
	bridge := slog.NewLogLogger(handler, level)
	log.SetFlags(0)
	log.SetOutput(bridge.Writer())

	return logger
}

// parseLevel maps the user-facing LOG_LEVEL values to slog
// constants. Unknown values fall back to info — log configuration
// shouldn't be the thing that crashes a pod on startup.
func parseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		// Includes "" (unset) and "info" — both should land on
		// info. Anything else is silently treated as info; a
		// loud one-time warning would just clutter every pod's
		// boot logs without giving operators anything actionable.
		return slog.LevelInfo
	}
}

// newHandler picks JSON vs text based on LOG_FORMAT. JSON is the
// default because every production log shipper indexes JSON for
// free; text is only useful when a developer is tailing the log
// from their terminal.
func newHandler(w io.Writer, level slog.Level) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))) {
	case "text":
		return slog.NewTextHandler(w, opts)
	default:
		return slog.NewJSONHandler(w, opts)
	}
}

// FromContext returns the logger attached to ctx, or slog.Default
// if none was attached. This is the function HTTP handlers and
// service methods call to log — never use slog.Default() directly
// from a code path that has a ctx in scope, otherwise the
// request_id / workspace_id correlation gets lost.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}
	if l, ok := ctx.Value(loggerCtxKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithContext returns a new ctx carrying logger as the value
// returned by FromContext. Used by Middleware after building a
// per-request child logger; can also be used by background
// workers that want to scope a logger to a job (e.g. "job_id" or
// "workspace_id" attached for the duration of one message
// handler).
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerCtxKey, logger)
}

// Middleware attaches a request-scoped logger to every request
// context. The child logger carries:
//
//   - http_method: GET / POST / etc.
//   - http_path: raw URL path (NOT the chi route pattern). chi
//     populates RouteContext.RoutePattern only after routing has
//     completed, so attempting to read it inside this middleware
//     yields an empty string. Operators that need to aggregate
//     by pattern should use the AccessLog middleware, which runs
//     post-dispatch and emits a per-request summary record with
//     the resolved pattern, status, and duration.
//   - remote_addr: helpful for rate-limit / abuse triage.
//   - request_id: set if the upstream sent X-Request-Id (chi's
//     middleware.RequestID also propagates it via
//     middleware.GetReqID inside handlers; we just want the
//     header value here so the access log and handler logs share
//     a single correlation id without depending on chi internals).
//
// Workspace and user IDs are added later by the auth middleware
// once JWT claims are resolved — those need to be additive, not
// replace the request-scoped fields.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := FromContext(r.Context())
		attrs := []any{
			"http_method", r.Method,
			"http_path", r.URL.Path,
			"remote_addr", clientIP(r),
		}
		// Prefer chi's middleware.RequestID context value, which
		// is set by chimw.RequestID earlier in the chain and
		// includes server-generated IDs for upstream clients
		// that didn't supply one. Fall back to the raw header
		// in case the chi middleware isn't installed (e.g.
		// tests using bare net/http).
		if rid := chimw.GetReqID(r.Context()); rid != "" {
			attrs = append(attrs, "request_id", rid)
		} else if rid := r.Header.Get("X-Request-Id"); rid != "" {
			attrs = append(attrs, "request_id", rid)
		}
		ctx := WithContext(r.Context(), base.With(attrs...))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AccessLog returns a middleware that emits a single "http
// request" info record per request AFTER the handler finishes,
// with the resolved chi route pattern, response status, response
// byte count, and request duration. Install this OUTSIDE the chi
// router (e.g. wrap the entire mux) so it observes both routed
// and unrouted requests.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Install a fresh chi RouteContext on the request ctx
		// BEFORE handing off to the inner chi router. chi will
		// populate this context in-place during routing rather
		// than creating its own, so when the handler chain
		// returns we can read the resolved route pattern off
		// the same struct. Without this, chi's internal
		// RouteContext lives on the child request that gets
		// scoped to the handler chain and is invisible to us.
		rctx := chi.NewRouteContext()
		ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
		r = r.WithContext(ctx)

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := timeNow()
		next.ServeHTTP(sw, r)
		dur := timeSince(start)

		pattern := r.URL.Path
		if p := rctx.RoutePattern(); p != "" {
			pattern = p
		}

		FromContext(r.Context()).Info("http request",
			"http_status", sw.status,
			"http_bytes", sw.bytes,
			"http_route", pattern,
			"duration_ms", dur.Milliseconds(),
		)
	})
}

// statusWriter wraps http.ResponseWriter to capture the response
// status code and byte count for the access log. WriteHeader may
// not be called explicitly (the net/http server defaults to 200),
// so the zero value defaults to 200 OK.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

// clientIP returns the request's remote IP, preferring the first
// entry in X-Forwarded-For when present (we sit behind a
// load-balancer / reverse proxy in every production deployment).
// Falls back to RemoteAddr without the port suffix.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if r.RemoteAddr == "" {
		return ""
	}
	// RemoteAddr is host:port; strip the port to keep the field
	// stable across reconnects on the same client.
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
