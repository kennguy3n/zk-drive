package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns the http.Handler that scrapes this Metrics
// registry as the standard Prometheus text-format response.
// Mount this at /metrics in the API server (operators can scrape
// it from their Prometheus job).
//
// Posture: the endpoint is intentionally NOT authenticated. The
// Go runtime + process collectors expose modest internal state
// (heap usage, goroutine count, open FDs, CPU time) which is
// fine for an operator-network scrape but should be firewalled
// off from the public internet. See the README's "Deploying"
// section for the recommended Network Policy / Ingress posture.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		// EnableOpenMetrics: false on purpose — Prometheus
		// itself parses the legacy text format just fine, and
		// disabling OpenMetrics shrinks the response by a few
		// bytes per series. Operators on an OpenMetrics-only
		// scraper (rare today) can flip this manually.
		EnableOpenMetrics: false,
	})
}

// HTTPMiddleware wraps an http.Handler with the request counters,
// duration histogram, and in-flight gauge defined on Metrics. It
// is intended to be installed inside the chi router via r.Use(...)
// so that chi has already populated chi.RouteContext by the time
// the wrapped handler returns — that's what gives us the bounded-
// cardinality route label.
//
// Placement vs. chi's Recoverer: HTTPMiddleware MUST be registered
// BEFORE chimw.Recoverer in r.Use(...) so that the resulting chain
// is HTTPMiddleware(Recoverer(handler)). With that ordering:
//
//  1. HTTPMiddleware wraps the response writer in `ww` and passes
//     `ww` down the chain — Recoverer receives `ww` and writes its
//     500-on-panic response through it, so `ww.Status()` correctly
//     reflects the 500 by the time HTTPMiddleware reads it.
//  2. If a panic ever escapes Recoverer (e.g. http.ErrAbortHandler,
//     which Recoverer re-panics by design), the deferred emission
//     below still runs so the counter + histogram fire with whatever
//     status `ww` saw last — better than the call vanishing entirely.
//
// The reversed ordering (Recoverer outer, HTTPMiddleware inner) is
// a latent bug: Recoverer writes the 500 to the original `w` (not
// `ww`, which it never sees), and on top of that the post-dispatch
// emission below was unreachable on panic. The defer pattern here
// belts-and-braces against future re-ordering accidents.
//
// Important: this middleware MUST run inside chi (i.e. attached
// via r.Use, not wrapping the chi mux from the outside) for
// RoutePattern() to return a non-empty value on matched routes.
// chi resolves the route pattern in-place on the RouteContext
// during its own tree walk; an outer middleware sees the same
// RouteContext after dispatch and can read RoutePattern() then.
//
// Unmatched paths (chi NotFoundHandler — 404s) report
// RoutePattern() = "" — we coerce to the literal string
// "not_matched" so 404 storms on attacker-discovered URLs
// (/wp-admin, /.git, etc.) cannot mint unbounded new series.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrap the response writer so we can read the chosen
		// status code AFTER the handler runs. Use chi's
		// WrapResponseWriter (same as logging.AccessLog) so
		// the Hijacker / Flusher / ReaderFrom optional
		// interfaces remain available to inner handlers —
		// WebSocket upgrades and SSE streams type-assert on
		// these, and a naive struct-embedding wrapper would
		// silently strip them and break the upgrade with a
		// 500.
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

		m.httpInFlightRequests.Inc()
		start := time.Now()

		// Emit counters + histogram in a defer so a panic
		// escaping the inner handler (or Recoverer re-panicking
		// http.ErrAbortHandler) still records the request. The
		// status read here reflects whatever Recoverer (or the
		// handler) wrote to ww before the panic unwound. Note on
		// the zero-status case: chi's basicWriter.Status() returns
		// the zero value (0), not 200, when neither WriteHeader
		// nor Write was called on the wrapper — Write() does
		// implicitly call WriteHeader(200) via maybeWriteHeader,
		// but WebSocket / SSE handlers that hijack the connection
		// (gorilla/websocket Upgrader, the chi Hijacker pathway)
		// never call Write on the wrapped writer, so ww.Status()
		// for those calls is 0. That's not a bug — it produces a
		// single bounded extra label value (status="0") that an
		// operator can recognise as "hijacked / upgraded
		// connection" — but the comment used to mis-state it as
		// "200 by default", which it isn't. The defer fires
		// regardless of which value Status() reports.
		defer func() {
			m.httpInFlightRequests.Dec()
			dur := time.Since(start).Seconds()
			route := routeLabel(r)
			status := strconv.Itoa(ww.Status())
			m.httpRequestsTotal.WithLabelValues(r.Method, route, status).Inc()
			m.httpRequestDuration.WithLabelValues(r.Method, route).Observe(dur)
		}()

		next.ServeHTTP(ww, r)
	})
}

// routeLabel extracts the chi RoutePattern (e.g.
// "/api/files/{fileID}") from the request context, falling back
// to the bounded sentinel "not_matched" for 404s. This is the
// load-bearing cardinality guard for the HTTP metrics: a naive
// implementation that used r.URL.Path would mint a fresh series
// for every distinct UUID a client sends, exhausting the scrape
// store in minutes.
func routeLabel(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return "not_matched"
}
