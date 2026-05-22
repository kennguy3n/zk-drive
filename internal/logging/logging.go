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
	"github.com/google/uuid"
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
	//
	// Bridge records are emitted at INFO regardless of LOG_LEVEL
	// because the producers are typically third-party libraries
	// (nats.go reconnect notices, http.Server connection errors,
	// etc.) that mean their `log.Printf` calls to be informational.
	// Letting LOG_LEVEL push them to ERROR would misrepresent the
	// severity for log aggregators that alert on ERROR rate; pushing
	// them to DEBUG would silently hide useful operator telemetry
	// the moment LOG_LEVEL switches back to INFO.
	bridge := slog.NewLogLogger(handler, slog.LevelInfo)
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

// Middleware is the in-chi-router companion to AccessLog. It is a
// no-op when AccessLog has already seeded the request-scoped
// logger (the production wiring in cmd/server installs AccessLog
// at the http.Server.Handler boundary, which means the logger is
// already populated by the time the chi router dispatches). It
// remains exported so binaries that do NOT wrap their mux with
// AccessLog (e.g. small internal services, tests using bare
// net/http) can still get the correlation attributes attached.
//
// When invoked without a pre-seeded logger, Middleware attaches:
//
//   - http_method, http_path, remote_addr
//   - request_id: read from chi's middleware.RequestID context
//     value first, then from X-Request-Id header. If neither
//     supplies one, generate a fresh UUID so handler logs always
//     carry a correlation id.
//
// Workspace and user IDs are added later by the auth middleware
// once JWT claims are resolved.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// If AccessLog (or any earlier wrapper) already seeded
		// the request-scoped logger we leave it alone — adding
		// the same fields again would create duplicate JSON
		// keys in the emitted record and confuse downstream
		// log aggregators.
		if _, ok := ctx.Value(loggerCtxKey).(*slog.Logger); ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx = withRequestScopedLogger(ctx, r, FromContext(ctx))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AccessLog wraps the entire mux and is responsible for two
// things, in this order:
//
//  1. Seeding the per-request correlation surface BEFORE
//     dispatch: a request_id (read from X-Request-Id or
//     generated), the request-scoped *slog.Logger carrying
//     http_method / http_path / remote_addr / request_id, AND a
//     fresh chi.RouteContext so post-dispatch we can read the
//     resolved route pattern off the same struct that chi
//     populated in-place.
//  2. Emitting one "http request" info record AFTER the handler
//     finishes, with http_status / http_bytes / http_route /
//     duration_ms attached to the same logger the handler used.
//
// Seeding correlation pre-dispatch is what makes a single
// request_id appear on every log line — including the access
// log record itself, which lives OUTSIDE the chi router and
// therefore cannot observe context modifications made by inner
// middleware (those happen on chi-internal request copies that
// never propagate back up). Without this seeding, the access
// log line would carry no request_id and operators couldn't
// correlate it to the handler-emitted logs from the same
// request.
//
// Install AccessLog outside chi so it observes routed AND
// unrouted requests (404s, recovered panics, etc.).
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Pre-install a chi.RouteContext so the inner router
		// populates it in-place during dispatch and we can
		// read RoutePattern() after next.ServeHTTP returns.
		rctx := chi.NewRouteContext()
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

		// Resolve request_id once, here, so the access log and
		// every handler log line carry the same id. chi's own
		// middleware.RequestID is intentionally NOT in the
		// chain anymore (it would generate a different id
		// inside chi that AccessLog couldn't see) — we set
		// chimw.RequestIDKey ourselves so chimw.GetReqID
		// continues to work for any handler that calls it.
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = uuid.NewString()
		}
		ctx = context.WithValue(ctx, chimw.RequestIDKey, reqID)
		ctx = withRequestScopedLogger(ctx, r, FromContext(ctx))

		// Wrap the response writer through chi's WrapResponseWriter
		// which preserves http.Hijacker / http.Flusher / io.ReaderFrom
		// via interface composition — critical for WebSocket
		// upgrades (gorilla/websocket type-asserts to Hijacker)
		// and SSE streams (assert to Flusher). A naive
		// status-capturing struct that only embeds
		// http.ResponseWriter silently strips these optional
		// interfaces and the upgrade fails with a 500.
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

		start := timeNow()
		next.ServeHTTP(ww, r.WithContext(ctx))
		dur := timeSince(start)

		pattern := r.URL.Path
		if p := rctx.RoutePattern(); p != "" {
			pattern = p
		}

		// FromContext(ctx) gives us the logger we seeded
		// pre-dispatch — same request_id as every handler log
		// line, so correlation works in log aggregators that
		// filter on request_id.
		FromContext(ctx).Info("http request",
			"http_status", ww.Status(),
			"http_bytes", ww.BytesWritten(),
			"http_route", pattern,
			"duration_ms", dur.Milliseconds(),
		)
	})
}

// withRequestScopedLogger derives a child logger from base
// carrying http_method, http_path, remote_addr, and request_id,
// then attaches it to ctx so FromContext returns it. request_id
// is sourced from (in priority order) the chimw.RequestIDKey
// value, the X-Request-Id header, then a freshly-generated
// UUID. Shared between AccessLog and Middleware so the
// two paths produce identical attribute sets.
func withRequestScopedLogger(ctx context.Context, r *http.Request, base *slog.Logger) context.Context {
	reqID := ""
	if rid := chimw.GetReqID(ctx); rid != "" {
		reqID = rid
	} else if rid := r.Header.Get("X-Request-Id"); rid != "" {
		reqID = rid
		ctx = context.WithValue(ctx, chimw.RequestIDKey, reqID)
	} else {
		reqID = uuid.NewString()
		ctx = context.WithValue(ctx, chimw.RequestIDKey, reqID)
	}
	return WithContext(ctx, base.With(
		"http_method", r.Method,
		"http_path", r.URL.Path,
		"remote_addr", clientIP(r),
		"request_id", reqID,
	))
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
