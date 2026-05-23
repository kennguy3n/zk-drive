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
	"sync"
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

const (
	loggerCtxKey ctxKey = 0
	slotCtxKey   ctxKey = 1
)

// requestLoggerSlot is the shared mutable container that
// AccessLog seeds into the request context pre-dispatch. Inner
// middleware in the chi chain (auth layering workspace_id /
// user_id / role, etc.) calls Enrich which swaps the slot's
// logger in place — so attributes added inside chi are visible
// to AccessLog's post-dispatch "http request" record AND to any
// goroutine spawned from the request that reads via FromContext
// after the swap. Without this slot the access log line would
// only carry attributes seeded BEFORE dispatch (request_id,
// http_method, http_path, remote_addr) because chi runs inner
// middleware on request copies whose context modifications
// never propagate back up to the outer http.Handler.
type requestLoggerSlot struct {
	mu  sync.RWMutex
	log *slog.Logger
}

func (s *requestLoggerSlot) get() *slog.Logger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.log
}

func (s *requestLoggerSlot) set(l *slog.Logger) {
	s.mu.Lock()
	s.log = l
	s.mu.Unlock()
}

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
	//
	// Use logger.Handler() (not the raw handler) so bridged records
	// carry the same "component" attribute every native slog call
	// emits. Without this, a third-party `log.Printf` from nats.go
	// would land in the log stream WITHOUT a component field,
	// breaking operators' filter-by-binary queries.
	bridge := slog.NewLogLogger(logger.Handler(), slog.LevelInfo)
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
//
// When ctx carries a requestLoggerSlot (i.e. the request went
// through AccessLog), FromContext returns the slot's current
// logger which reflects every attribute Enrich layered on
// during the chi dispatch. This is what makes post-dispatch
// AccessLog records carry workspace_id / user_id from the auth
// middleware that ran inside chi.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}
	// The nil-slot check supports the DetachForBackground sentinel
	// pattern: callers that want to break a child goroutine off
	// from the request's slot install a typed-nil slot here so the
	// next lookup falls through to loggerCtxKey instead of reading
	// (and racing with) the parent request's slot.
	if slot, ok := ctx.Value(slotCtxKey).(*requestLoggerSlot); ok && slot != nil {
		if l := slot.get(); l != nil {
			return l
		}
	}
	if l, ok := ctx.Value(loggerCtxKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithContext returns a new ctx carrying logger as the value
// returned by FromContext. Used by background workers that want
// to scope a logger to a job (e.g. "job_id" or "workspace_id"
// attached for the duration of one message handler).
//
// WithContext stores the logger immutably — sub-tasks branched
// from the returned ctx see this logger, but the parent ctx is
// unaffected. For request-scoped attribute enrichment that the
// access log line must also observe (auth middleware adding
// workspace_id / user_id), use Enrich instead.
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerCtxKey, logger)
}

// DetachForBackground returns a context suitable for a goroutine
// that out-lives the HTTP request it was spawned from. It captures
// the request-scoped slot's CURRENT logger snapshot (including any
// workspace_id / user_id / request_id that auth + access-log
// middleware threaded into it) and re-attaches that snapshot via
// WithContext on a context that no longer holds the slot.
//
// Why this exists: callers that want to enrich a detached
// goroutine's logger (e.g. add an invite_id correlation for an
// async email send) cannot use Enrich — Enrich would mutate the
// request's slot from another goroutine, which races with the
// outer AccessLog frame's slot read AND would leak the
// goroutine-only attribute into the access log line of the
// INBOUND HTTP request. They also cannot use WithContext directly
// against the request context: FromContext checks slotCtxKey FIRST
// and returns the slot's (unenriched) logger, silently shadowing
// the WithContext-set logger at loggerCtxKey.
//
// DetachForBackground closes that gap: after this call, the
// returned context has the snapshot at loggerCtxKey AND a typed-nil
// at slotCtxKey, so FromContext skips the slot branch and falls
// through to loggerCtxKey. Subsequent WithContext / Enrich calls
// against the returned context behave like any non-HTTP context —
// they don't touch the request's slot, and they don't race.
//
// The returned context still carries all OTHER values from ctx
// (workspace_id, user_id from tenantctx, anything else
// middleware-attached). Callers typically pair this with
// context.WithoutCancel + context.WithTimeout to also detach the
// cancellation lifecycle.
func DetachForBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	logger := FromContext(ctx)
	// Shadow the slot with a typed nil so FromContext's type
	// assertion (*requestLoggerSlot) succeeds but the nil check
	// at line 199 falls through to the loggerCtxKey branch.
	ctx = context.WithValue(ctx, slotCtxKey, (*requestLoggerSlot)(nil))
	return WithContext(ctx, logger)
}

// Enrich layers attrs onto the request-scoped logger so they
// appear on every subsequent log line emitted via
// FromContext(ctx) — INCLUDING the "http request" access log
// record that AccessLog emits after the chi handler chain
// returns. Used by middleware running inside chi (notably the
// auth middleware) that needs its enrichments to be visible at
// the http.Handler boundary outside chi.
//
// When ctx has a request-scoped slot (i.e. AccessLog seeded one
// pre-dispatch), Enrich swaps the slot's logger in place and
// returns ctx unchanged — the mutation is what makes the
// enrichment visible to the outer AccessLog frame.
//
// When ctx does NOT have a slot (background workers, tests,
// code paths that never went through AccessLog), Enrich falls
// back to WithContext so callers can use this function safely
// in both HTTP and non-HTTP contexts.
func Enrich(ctx context.Context, attrs ...any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(attrs) == 0 {
		return ctx
	}
	// Mirror FromContext's nil-slot guard so an Enrich call on a
	// DetachForBackground'd context attaches via WithContext
	// (the non-HTTP branch) instead of trying to mutate the
	// typed-nil sentinel and panicking on a nil-receiver RLock.
	if slot, ok := ctx.Value(slotCtxKey).(*requestLoggerSlot); ok && slot != nil {
		base := slot.get()
		if base == nil {
			base = FromContext(ctx)
		}
		slot.set(base.With(attrs...))
		return ctx
	}
	return WithContext(ctx, FromContext(ctx).With(attrs...))
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

// AccessLog wraps the entire mux and is responsible for three
// things, in this order:
//
//  1. Seeding the per-request correlation surface BEFORE
//     dispatch: a request_id (read from X-Request-Id or
//     generated), the request-scoped *slog.Logger carrying
//     http_method / http_path / remote_addr / request_id, AND a
//     fresh chi.RouteContext so post-dispatch we can read the
//     resolved route pattern off the same struct that chi
//     populated in-place.
//  2. Installing a request-scoped logger slot that inner
//     middleware (auth) mutates via Enrich, so workspace_id /
//     user_id / role layered on inside chi are visible to the
//     access log record emitted outside chi.
//  3. Emitting one "http request" info record AFTER the handler
//     finishes, with http_status / http_bytes / http_route /
//     duration_ms attached to the SAME logger the handler used
//     (including any attrs Enrich layered on during dispatch).
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
		reqID := sanitizeRequestID(r.Header.Get("X-Request-Id"))
		if reqID == "" {
			reqID = uuid.NewString()
		}
		ctx = context.WithValue(ctx, chimw.RequestIDKey, reqID)

		// Echo the resolved request_id back to the client via the
		// X-Request-Id response header. This is the same contract
		// chimw.RequestID provided before WS-9 — clients that record
		// the server-assigned id from their HTTP response can use it
		// to correlate a client-side error report to the server-side
		// log line in tools like Datadog / Honeycomb. Without this
		// header, the access log's request_id becomes a private
		// server-only field with no path back to the client.
		w.Header().Set("X-Request-Id", reqID)

		// Build the request-scoped logger AND install the
		// shared slot so inner chi middleware can mutate it
		// via Enrich (auth middleware uses this path to attach
		// workspace_id / user_id / role after JWT validation).
		base := FromContext(ctx).With(
			"http_method", r.Method,
			"http_path", r.URL.Path,
			"remote_addr", clientIP(r),
			"request_id", reqID,
		)
		slot := &requestLoggerSlot{log: base}
		ctx = context.WithValue(ctx, slotCtxKey, slot)
		ctx = context.WithValue(ctx, loggerCtxKey, base)

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
// UUID. Used by Middleware when running standalone (no
// AccessLog wrapper); AccessLog has its own inline equivalent
// because it ALSO needs to install the mutable logger slot.
func withRequestScopedLogger(ctx context.Context, r *http.Request, base *slog.Logger) context.Context {
	reqID := ""
	if rid := chimw.GetReqID(ctx); rid != "" {
		reqID = rid
	} else if rid := sanitizeRequestID(r.Header.Get("X-Request-Id")); rid != "" {
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

// clientIP returns the request's remote IP using the same
// precedence chi's middleware.RealIP applies
// (True-Client-IP → X-Real-IP → X-Forwarded-For → RemoteAddr).
// Matching that order ensures the access log's `remote_addr`
// matches what downstream handlers see after chi.RealIP has
// rewritten r.RemoteAddr, so a dashboard `WHERE remote_addr =
// '...'` query joins consistently across the access log and the
// audit log regardless of which header the upstream proxy uses
// (Cloudflare emits True-Client-IP, nginx is often configured
// for X-Real-IP, AWS ALB / GCP LB / Fastly use X-Forwarded-For).
func clientIP(r *http.Request) string {
	if tcip := r.Header.Get("True-Client-IP"); tcip != "" {
		return strings.TrimSpace(tcip)
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
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
	// stable across reconnects on the same client. Also strip
	// surrounding "[" / "]" so the IPv6 form "[::1]:5" becomes
	// "::1", matching internal/audit/service.go's clientIP.
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return host
}

// maxRequestIDLen caps the X-Request-Id header value before it
// is embedded in every log line for the request. Without a cap,
// a malicious or buggy upstream could send a multi-megabyte
// header that gets amplified into every record — inflating log
// storage costs and slowing log ingestion. 256 bytes is more
// than enough for the formats every real correlation system
// uses (UUIDv4 = 36, Honeycomb trace-id = 32, Datadog = 19,
// AWS X-Ray = 35, W3C traceparent value = 55).
const maxRequestIDLen = 256

// sanitizeRequestID validates an externally-supplied X-Request-Id
// before we accept it as the request correlation id. The id must
// be ≤ maxRequestIDLen bytes and consist only of printable ASCII
// (excluding whitespace and control characters) — strict enough
// to prevent log-injection via embedded newlines / JSON quote
// characters / shell metacharacters in operator copy-paste, and
// loose enough to accept every real-world format we've seen
// (UUIDs, hex trace ids, base64 trace ids, slash-delimited
// vendor formats). Returns "" when the header is missing,
// over-long, or contains disallowed bytes — callers fall back to
// uuid.NewString() in that case.
func sanitizeRequestID(raw string) string {
	if raw == "" || len(raw) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 0x21 || c > 0x7e {
			return ""
		}
	}
	return raw
}
