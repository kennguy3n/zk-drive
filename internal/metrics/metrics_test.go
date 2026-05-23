package metrics_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kennguy3n/zk-drive/internal/metrics"
	"github.com/kennguy3n/zk-drive/internal/reconciler"
)

// TestNew_RegistryIsolated verifies that two Metrics values do
// NOT collide on the global default registry. Each constructor
// call must mint a fresh prometheus.Registry, or test isolation
// breaks the moment two test packages link metrics together.
func TestNew_RegistryIsolated(t *testing.T) {
	a := metrics.New()
	b := metrics.New()
	if a.Registry == b.Registry {
		t.Fatalf("two New() calls share a Registry; the package is using the default registerer somewhere")
	}
	if _, err := a.Registry.Gather(); err != nil {
		t.Fatalf("a.Registry.Gather: %v", err)
	}
	if _, err := b.Registry.Gather(); err != nil {
		t.Fatalf("b.Registry.Gather: %v", err)
	}
}

// TestNew_ExposesDefaultCollectors verifies the Go runtime + process
// collectors land on the registry. We check that at least one
// family from each (go_*, process_*) is present.
func TestNew_ExposesDefaultCollectors(t *testing.T) {
	m := metrics.New()
	families := mustGather(t, m.Registry)
	if len(families) == 0 {
		t.Fatalf("Gather returned zero metric families; default collectors did not register")
	}
	wantPrefixes := []string{"go_", "process_"}
	have := map[string]bool{}
	for _, fam := range families {
		for _, p := range wantPrefixes {
			if strings.HasPrefix(fam.GetName(), p) {
				have[p] = true
			}
		}
	}
	for _, p := range wantPrefixes {
		if !have[p] {
			t.Errorf("expected at least one family with prefix %q, got none. Families: %v", p, familyNames(families))
		}
	}
}

// TestHTTPMiddleware_LabelsByRoutePattern verifies the load-bearing
// cardinality guard: requests to /api/files/abc and /api/files/def
// MUST share the route="/api/files/{fileID}" label rather than mint
// two distinct series. A naive r.URL.Path-based implementation
// would fail this test instantly.
func TestHTTPMiddleware_LabelsByRoutePattern(t *testing.T) {
	m := metrics.New()
	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	r.Get("/api/files/{fileID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, fileID := range []string{"abc", "def", "ghi"} {
		req := httptest.NewRequest(http.MethodGet, "/api/files/"+fileID, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}

	got := counterValue(t, m.Registry, "zkdrive_http_requests_total", map[string]string{
		"method": "GET",
		"route":  "/api/files/{fileID}",
		"status": "200",
	})
	if got != 3 {
		t.Errorf("zkdrive_http_requests_total{route=/api/files/{fileID}} = %v; want 3", got)
	}

	// Also verify no per-fileID series leaked in. Enumerating
	// the metric children gives us a direct cardinality check.
	if seriesCount := countMetricsInFamily(t, m.Registry, "zkdrive_http_requests_total"); seriesCount != 1 {
		t.Errorf("http_requests_total has %d series; want 1 (route pattern should be the only label-tuple)", seriesCount)
	}
}

// TestHTTPMiddleware_NotMatchedLabel verifies the 404 path uses
// the bounded "not_matched" sentinel. Without this guard, a script
// kiddie hitting random URLs (/wp-admin, /.git, /api/v0/users/{guid})
// would mint unbounded new series per scrape window.
func TestHTTPMiddleware_NotMatchedLabel(t *testing.T) {
	m := metrics.New()
	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	// chi short-circuits the middleware chain when there are
	// zero registered routes (because the router knows up front
	// there's nothing to match). Register a decoy route so the
	// middleware actually fires on unmatched paths — which is
	// the production reality, where the server has dozens of
	// routes installed.
	r.Get("/decoy", func(w http.ResponseWriter, _ *http.Request) {})

	for _, path := range []string{"/wp-admin", "/.git/config", "/api/v0/users/abc"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("path %q: status = %d, want 404", path, w.Code)
		}
	}

	got := counterValue(t, m.Registry, "zkdrive_http_requests_total", map[string]string{
		"method": "GET",
		"route":  "not_matched",
		"status": "404",
	})
	if got != 3 {
		t.Errorf("zkdrive_http_requests_total{route=not_matched} = %v; want 3", got)
	}
}

// TestHTTPMiddleware_RecordsOnPanic pins the post-fix invariant
// for the Recoverer ordering: when the chain is
// HTTPMiddleware(Recoverer(handler)) and the handler panics, the
// counter must still increment with status="500" (Recoverer writes
// the 500 through the wrapped writer) AND the in-flight gauge
// must decrement back to zero. The pre-fix ordering
// (Recoverer outer) silently dropped panicked requests from the
// metrics surface — this test guards against a regression to that.
func TestHTTPMiddleware_RecordsOnPanic(t *testing.T) {
	m := metrics.New()
	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	r.Use(chimw.Recoverer)
	r.Get("/boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("synthetic panic in handler")
	})

	// Recoverer logs the stack to its default print sink. We can't
	// silence that cleanly across versions, so just accept the
	// noise — the assertions below are what matter.
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("Recoverer status = %d, want 500", w.Code)
	}

	got := counterValue(t, m.Registry, "zkdrive_http_requests_total", map[string]string{
		"method": "GET",
		"route":  "/boom",
		"status": "500",
	})
	if got != 1 {
		t.Errorf("zkdrive_http_requests_total{status=500} = %v; want 1", got)
	}

	if g := gaugeValue(t, m.Registry, "zkdrive_http_in_flight_requests"); g != 0 {
		t.Errorf("in-flight gauge = %v after panicked request, want 0", g)
	}
}

// TestHTTPMiddleware_InFlightGauge verifies the gauge decrements
// after each request. We can't easily observe the mid-request
// peak in a unit test (would require goroutine choreography) so
// we check the post-condition: gauge == 0 after the handler
// returns.
func TestHTTPMiddleware_InFlightGauge(t *testing.T) {
	m := metrics.New()
	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := gaugeValue(t, m.Registry, "zkdrive_http_in_flight_requests")
	if got != 0 {
		t.Errorf("in-flight gauge = %v after request, want 0", got)
	}
}

// TestInstrumentJob_CountsByResult verifies the worker wrapper
// emits the right (subject, result) label tuple on each return
// path. Every JobResult value must be observable through the
// counter — otherwise a handler that returns a not-yet-supported
// result string would silently drop on the floor.
func TestInstrumentJob_CountsByResult(t *testing.T) {
	m := metrics.New()
	subject := "drive.test"

	results := []metrics.JobResult{
		metrics.JobResultOK,
		metrics.JobResultOK,
		metrics.JobResultSkip,
		metrics.JobResultError,
		metrics.JobResultDropped,
	}
	for _, r := range results {
		want := r // pin in closure
		wrapped := m.InstrumentJob(context.Background(), subject, func(_ context.Context, _ *nats.Msg) metrics.JobResult {
			return want
		})
		wrapped(&nats.Msg{Subject: subject})
	}

	cases := []struct {
		result metrics.JobResult
		want   float64
	}{
		{metrics.JobResultOK, 2},
		{metrics.JobResultSkip, 1},
		{metrics.JobResultError, 1},
		{metrics.JobResultDropped, 1},
	}
	for _, c := range cases {
		got := counterValue(t, m.Registry, "zkdrive_worker_jobs_total", map[string]string{
			"subject": subject,
			"result":  string(c.result),
		})
		if got != c.want {
			t.Errorf("zkdrive_worker_jobs_total{subject=%q,result=%q} = %v; want %v", subject, c.result, got, c.want)
		}
	}
}

// TestInstrumentJob_DurationObserved verifies the histogram gets
// at least one observation per call. We don't assert on the
// bucket values (timing-sensitive across CI runners) — just on
// the sample count, which is deterministic.
func TestInstrumentJob_DurationObserved(t *testing.T) {
	m := metrics.New()
	subject := "drive.test"

	wrapped := m.InstrumentJob(context.Background(), subject, func(_ context.Context, _ *nats.Msg) metrics.JobResult {
		return metrics.JobResultOK
	})
	for i := 0; i < 5; i++ {
		wrapped(&nats.Msg{})
	}

	got := histogramCount(t, m.Registry, "zkdrive_worker_job_duration_seconds", map[string]string{
		"subject": subject,
	})
	if got != 5 {
		t.Errorf("sample count = %d, want 5", got)
	}
}

// TestRecordReconcilerRun_ResultClassification pins the err →
// result-label mapping (ok / cancelled / error) so a future
// refactor of the helper can't silently flip the contract.
func TestRecordReconcilerRun_ResultClassification(t *testing.T) {
	m := metrics.New()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil err is ok", nil, "ok"},
		{"context.Canceled is cancelled", context.Canceled, "cancelled"},
		{"context.DeadlineExceeded is cancelled", context.DeadlineExceeded, "cancelled"},
		{"wrapped context.Canceled is cancelled", errors.Join(errors.New("driver shutdown"), context.Canceled), "cancelled"},
		{"other err is error", errors.New("boom"), "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.RecordReconcilerRun(reconciler.Summary{}, c.err, time.Now().Add(-10*time.Millisecond))
		})
	}

	for _, c := range cases {
		got := counterValue(t, m.Registry, "zkdrive_reconciler_runs_total", map[string]string{
			"result": c.want,
		})
		if got < 1 {
			t.Errorf("zkdrive_reconciler_runs_total{result=%q} = %v; want >= 1", c.want, got)
		}
	}
}

// TestRecordReconcilerRun_AggregatesSummary verifies the helper
// folds the Summary counts into the right counters (workspaces
// scanned / updated, drift bytes, per-workspace errors).
func TestRecordReconcilerRun_AggregatesSummary(t *testing.T) {
	m := metrics.New()
	summary := reconciler.Summary{
		Workspaces:      10,
		Updated:         3,
		TotalDriftBytes: 4096,
		Errors: []reconciler.WorkspaceError{
			{Err: errors.New("workspace 1 failed")},
			{Err: errors.New("workspace 7 failed")},
		},
	}
	m.RecordReconcilerRun(summary, nil, time.Now().Add(-5*time.Millisecond))

	if got := counterValue(t, m.Registry, "zkdrive_reconciler_workspaces_scanned_total", nil); got != 10 {
		t.Errorf("workspaces_scanned = %v; want 10", got)
	}
	if got := counterValue(t, m.Registry, "zkdrive_reconciler_workspaces_updated_total", nil); got != 3 {
		t.Errorf("workspaces_updated = %v; want 3", got)
	}
	if got := counterValue(t, m.Registry, "zkdrive_reconciler_drift_bytes_total", nil); got != 4096 {
		t.Errorf("drift_bytes = %v; want 4096", got)
	}
	if got := counterValue(t, m.Registry, "zkdrive_reconciler_workspace_errors_total", nil); got != 2 {
		t.Errorf("workspace_errors = %v; want 2", got)
	}
}

// TestHandler_ScrapeRoundTrip exercises the full /metrics export
// path: install Metrics, fire one HTTP request through the
// middleware, then assert that the /metrics body contains the
// expected series name + label tuple. This is the end-to-end
// integration check that promhttp.HandlerFor wires up to the
// custom Registry correctly.
func TestHandler_ScrapeRoundTrip(t *testing.T) {
	m := metrics.New()
	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	r.Get("/api/files/{fileID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/metrics", m.Handler().ServeHTTP)

	req := httptest.NewRequest(http.MethodGet, "/api/files/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	mReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mW := httptest.NewRecorder()
	r.ServeHTTP(mW, mReq)

	if mW.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", mW.Code)
	}
	body, _ := io.ReadAll(mW.Body)
	if !strings.Contains(string(body), `zkdrive_http_requests_total{method="GET",route="/api/files/{fileID}",status="200"} 1`) {
		t.Errorf("/metrics body missing expected counter line; body:\n%s", body)
	}
}

// TestHandler_ScrapeIntegratesAllSurfaces is the cross-cutting
// integration check that pins the "single Metrics value drives
// HTTP middleware + worker wrapper + reconciler hook simultaneously"
// contract. A regression here means one of the three flows landed
// on a different prometheus.Registry than the /metrics endpoint
// is scraping — exactly the foot-gun the public Metrics value
// exists to prevent.
func TestHandler_ScrapeIntegratesAllSurfaces(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	r.Get("/api/files/{fileID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/metrics", m.Handler().ServeHTTP)

	// HTTP surface — one request through the middleware.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/files/abc", nil))

	// Worker surface — one InstrumentJob invocation.
	wrapped := m.InstrumentJob(context.Background(), "drive.preview.generate", func(_ context.Context, _ *nats.Msg) metrics.JobResult {
		return metrics.JobResultOK
	})
	wrapped(&nats.Msg{Data: []byte("{}")})

	// Reconciler surface — one RecordReconcilerRun call.
	m.RecordReconcilerRun(reconciler.Summary{
		Workspaces:      5,
		Updated:         2,
		TotalDriftBytes: 1024,
	}, nil, time.Now().Add(-50*time.Millisecond))

	// Email surface — one RecordEmailSent call. Bounded labels.
	m.RecordEmailSent("guest_invite", "ok")

	mReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mW := httptest.NewRecorder()
	r.ServeHTTP(mW, mReq)
	if mW.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", mW.Code)
	}
	body, _ := io.ReadAll(mW.Body)
	bodyStr := string(body)

	expectedLines := []string{
		`zkdrive_http_requests_total{method="GET",route="/api/files/{fileID}",status="200"} 1`,
		`zkdrive_worker_jobs_total{result="ok",subject="drive.preview.generate"} 1`,
		`zkdrive_reconciler_runs_total{result="ok"} 1`,
		`zkdrive_reconciler_workspaces_scanned_total 5`,
		`zkdrive_reconciler_workspaces_updated_total 2`,
		`zkdrive_reconciler_drift_bytes_total 1024`,
		`zkdrive_email_sent_total{outcome="ok",template="guest_invite"} 1`,
	}
	for _, line := range expectedLines {
		if !strings.Contains(bodyStr, line) {
			t.Errorf("/metrics body missing expected line %q; body:\n%s", line, bodyStr)
		}
	}

	// Default collectors must still be present alongside the
	// app-specific families — confirms one Registry holds the
	// whole surface, not a separate "app" registry that lost
	// the runtime collectors.
	if !strings.Contains(bodyStr, "go_goroutines") {
		t.Errorf("/metrics body missing go_goroutines (default collector); body did not include runtime metrics")
	}
}

// ---- helpers --------------------------------------------------------

func mustGather(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return families
}

// counterValue reads the float64 value of the counter family
// matching `name` and (label name → label value) `labels`. A
// nil labels map matches any (and only one) series in the
// family — useful for label-free counters.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	for _, fam := range mustGather(t, reg) {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("counter %s with labels %v not found", name, labels)
	return 0
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	for _, fam := range mustGather(t, reg) {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %s not found", name)
	return 0
}

func histogramCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	for _, fam := range mustGather(t, reg) {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("histogram %s with labels %v not found", name, labels)
	return 0
}

func countMetricsInFamily(t *testing.T, reg *prometheus.Registry, name string) int {
	t.Helper()
	for _, fam := range mustGather(t, reg) {
		if fam.GetName() == name {
			return len(fam.GetMetric())
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	have := map[string]string{}
	for _, lp := range got {
		have[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func familyNames(families []*dto.MetricFamily) []string {
	out := make([]string, 0, len(families))
	for _, f := range families {
		out = append(out, f.GetName())
	}
	return out
}
