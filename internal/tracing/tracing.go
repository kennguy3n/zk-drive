// Package tracing wires OpenTelemetry distributed tracing for the
// zk-drive server and worker binaries.
//
// # Why OpenTelemetry (and not a vendor SDK)
//
// We already use two of the three observability pillars: structured
// JSON logs (slog + chi correlation) and Prometheus metrics.
// Distributed traces are the missing third pillar: they
// answer "which dependency added latency to THIS request" in a way
// that metrics can't (cardinality limits) and logs can't (no causal
// linkage). OpenTelemetry has been the CNCF-graduated standard since
// 2024, and the OTLP/HTTP wire format is the only ingest format
// shared by Jaeger, Grafana Tempo, Honeycomb, Datadog (via their
// gateway), New Relic, Splunk, SigNoz, and the open-source Grafana
// Alloy / OTel Collector pipelines. Picking OTel here keeps us off
// any single vendor's roadmap.
//
// # Graceful degrade
//
// When [Config.Endpoint] is empty, [Init] installs a tracer provider
// backed by a no-op exporter — span creation still works (so
// instrumented code keeps compiling cleanly without endpoint-checks
// at every call site), but no spans are exported. The startup log
// announces this state at INFO level so an operator knows tracing is
// inert without checking dashboards. This mirrors the SMTP graceful
// degrade pattern: missing config is logged once at boot, not
// surfaced as silent dropped data later.
//
// # Sampling
//
// The provider uses a parent-based ratio sampler: incoming requests
// already carrying a sampled trace context propagate the upstream
// decision (so a "force sample for this user" header set by a
// frontend or load balancer is honoured), while root spans use the
// configured ratio. Default 0.1 (10%) — production-safe budget;
// dev / staging operators set 1.0 for full visibility. A ratio of
// 0.0 disables root-span sampling without disabling the SDK, which
// is useful when you want propagation to work but exporter cost to
// be zero.
//
// # Resource attributes
//
// Every span carries:
//
//   - service.name (default "zk-drive")
//   - service.version (from internal/version.Version, set at build
//     time via -ldflags)
//   - deployment.environment (optional, e.g. "production")
//   - process.* and host.* (auto-detected from os / runtime)
//   - service.instance.id (random UUID per process — distinguishes
//     replicas within the same service)
//
// These are the conventional [OpenTelemetry resource semantic
// conventions], so any backend that knows OTel can group / filter
// without custom mapping.
//
// [OpenTelemetry resource semantic conventions]: https://opentelemetry.io/docs/specs/semconv/resource/
package tracing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config bundles the operator-tunable knobs for the tracer provider.
// Constructed by [BuildFromOperatorConfig] from the matching fields
// on internal/config.Config so the tracing package stays importable
// from places that don't pull the full server config in.
type Config struct {
	// ServiceName is the service.name resource attribute. Required;
	// [BuildFromOperatorConfig] defaults it to "zk-drive" when the
	// matching config field is empty.
	ServiceName string

	// ServiceVersion is the service.version resource attribute,
	// typically [internal/version.Version] which the build pipeline
	// stamps via -ldflags.
	ServiceVersion string

	// DeploymentEnvironment is the deployment.environment resource
	// attribute, e.g. "production". Optional — when empty the
	// attribute is omitted entirely rather than emitted as the
	// empty string (otherwise dashboards filtering on env="" would
	// collapse all unlabeled spans into a single bucket).
	DeploymentEnvironment string

	// InstanceID disambiguates replicas of the same service. When
	// empty [Init] generates a UUID per process at startup.
	InstanceID string

	// Endpoint is the OTLP/HTTP exporter base URL. When empty the
	// tracer provider is constructed in no-op mode (span calls are
	// cheap, nothing is exported). Both base URLs ("https://host:4318",
	// the package appends "/v1/traces") and full URLs are accepted.
	Endpoint string

	// Headers are forwarded as HTTP headers on every OTLP export
	// request. Typical use: vendor auth headers like
	// "x-honeycomb-team" or "Authorization: Bearer ...".
	Headers map[string]string

	// Insecure disables TLS on the exporter HTTP client. Only set
	// for an in-cluster collector reached over plain HTTP — never
	// for a managed backend on the public internet.
	Insecure bool

	// Compression: "gzip" (default) or "none". Maps directly to the
	// otlptracehttp.WithCompression option.
	Compression string

	// SamplerRatio is the root-span sampling probability in [0,1].
	// 0.1 (10%) is the safe production default; the WithSampler
	// wrapper is parent-based so already-sampled incoming traces
	// propagate the upstream decision regardless of this value.
	SamplerRatio float64

	// ExportTimeout is the per-batch upload timeout. The batch
	// processor's queue isolates this from the request hot path,
	// so a slow exporter does NOT add latency to user requests.
	// Defaults to 30s — long enough to survive transient backend
	// hiccups, short enough that a permanently-down backend doesn't
	// pin the export goroutine across hours of retries.
	ExportTimeout time.Duration
}

// DefaultExportTimeout is the per-batch upload timeout used when
// Config.ExportTimeout is zero. Exposed so tests can pin the value.
const DefaultExportTimeout = 30 * time.Second

// MaxSamplerRatio is the upper bound applied to Config.SamplerRatio
// before constructing the sampler. Values above 1.0 (which the OTel
// SDK would silently round to 1.0 on every sample call) are clamped
// at Init time and surfaced through a startup warning so operators
// see the typo.
const MaxSamplerRatio = 1.0

// BuildFromOperatorConfig maps an internal/config.Config (passed
// untyped to avoid an import cycle — the operator config package
// imports nothing from internal/tracing today, but a future cross-
// import would create a cycle) into a [Config] ready for [Init].
//
// version is read from internal/version.Version by the caller and
// passed in as a string so the tracing package stays free of the
// version import (which is otherwise specific to the binary that
// links the version stamp). Empty version is passed through to the
// resource builder, which omits the service.version attribute when
// the value is the default "dev" placeholder.
type OperatorConfig struct {
	Endpoint              string
	Headers               map[string]string
	Insecure              bool
	Compression           string
	ServiceName           string
	DeploymentEnvironment string
	SamplerRatio          float64
}

// BuildFromOperatorConfig converts the operator-facing config fields
// into a [Config]. The defaulting rules (empty service name → "zk-drive",
// negative or out-of-range sampler → 0.1, empty compression → "gzip")
// live here so [Init] can be a pure consumer.
func BuildFromOperatorConfig(oc OperatorConfig, version string) Config {
	name := strings.TrimSpace(oc.ServiceName)
	if name == "" {
		name = "zk-drive"
	}
	compression := strings.ToLower(strings.TrimSpace(oc.Compression))
	if compression == "" {
		compression = "gzip"
	}
	// Ratio is passed through unmodified — Init clamps invalid
	// values when constructing the SDK sampler so the startup
	// log can warn an operator who set OTEL_TRACES_SAMPLER_ARG
	// outside [0, 1] instead of silently coercing it here.
	ratio := oc.SamplerRatio
	return Config{
		ServiceName:           name,
		ServiceVersion:        version,
		DeploymentEnvironment: oc.DeploymentEnvironment,
		Endpoint:              strings.TrimSpace(oc.Endpoint),
		Headers:               oc.Headers,
		Insecure:              oc.Insecure,
		Compression:           compression,
		SamplerRatio:          ratio,
		ExportTimeout:         DefaultExportTimeout,
	}
}

// Provider bundles the SDK-level tracer provider with a [Shutdown]
// method that flushes pending spans and tears down the exporter.
// Callers register the returned shutdown function via defer so the
// process-exit path always drains the queue.
type Provider struct {
	tp     trace.TracerProvider
	shut   func(context.Context) error
	noop   bool
	reason string
}

// Tracer returns a tracer named after the caller's package. Prefer
// passing the import path of the calling package (e.g.
// "github.com/kennguy3n/zk-drive/internal/email") so spans group by
// origin in the backend.
func (p *Provider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return p.tp.Tracer(name, opts...)
}

// Shutdown flushes pending spans and tears down the exporter. Safe
// to call multiple times; the underlying SDK shutdown is idempotent.
// Returns nil for the no-op provider so callers don't need to branch
// on whether tracing was actually enabled.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.shut == nil {
		return nil
	}
	return p.shut(ctx)
}

// IsEnabled reports whether spans are actually exported. False for
// the no-op provider (returned when Config.Endpoint is empty); true
// for the OTLP-backed provider. Callers can use this to gate
// optional instrumentation that would be wasted on a no-op tracer
// (e.g. capturing large request bodies as span events).
func (p *Provider) IsEnabled() bool { return !p.noop }

// DisableReason describes why tracing is in no-op mode. Empty when
// IsEnabled returns true. Surfaced through [Provider.LogStartup] so
// operators see "OTEL_EXPORTER_OTLP_ENDPOINT not set" rather than
// having to grep config to figure out why no traces are arriving.
func (p *Provider) DisableReason() string { return p.reason }

// LogStartup emits a single info-level record summarising the
// effective tracer configuration. Run after [Init] and before the
// HTTP server starts accepting traffic so the log line sits next to
// the other "X enabled / disabled" boot records (database, redis,
// SMTP, etc.).
func (p *Provider) LogStartup(ctx context.Context) {
	if p.noop {
		slog.InfoContext(ctx, "tracing disabled",
			"reason", p.reason,
		)
		return
	}
	slog.InfoContext(ctx, "tracing enabled",
		"exporter", "otlphttp",
	)
}

// Init builds the tracer provider from cfg and installs it as the
// global provider via otel.SetTracerProvider. Returns a [*Provider]
// whose Shutdown the caller MUST defer so exit-time span flush runs.
//
// Errors are reserved for genuine setup failures (resource build,
// exporter construction). Endpoint="" is NOT an error — it short-
// circuits to a no-op provider and is the canonical "tracing
// disabled" path.
func Init(ctx context.Context, cfg Config) (*Provider, error) {
	// Empty endpoint installs the no-op provider. Span calls
	// remain valid so instrumented code keeps compiling, but
	// nothing exports. We still install the W3C propagator so a
	// future toggle to enabled doesn't need a code change in
	// callers that read trace context out of HTTP headers.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if strings.TrimSpace(cfg.Endpoint) == "" {
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		return &Provider{
			tp:     tp,
			shut:   nil,
			noop:   true,
			reason: "OTEL_EXPORTER_OTLP_ENDPOINT is not set",
		}, nil
	}

	instanceID := strings.TrimSpace(cfg.InstanceID)
	if instanceID == "" {
		instanceID = uuid.NewString()
	}

	// Resource attributes follow OpenTelemetry resource semantic
	// conventions. service.version is omitted when ServiceVersion
	// is empty or the placeholder "dev" (set by the version
	// package when -ldflags isn't applied) so dashboards don't
	// filter on a meaningless string.
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceInstanceID(instanceID),
	}
	if v := strings.TrimSpace(cfg.ServiceVersion); v != "" && v != "dev" {
		attrs = append(attrs, semconv.ServiceVersion(v))
	}
	if env := strings.TrimSpace(cfg.DeploymentEnvironment); env != "" {
		// semconv v1.26.0 still names this attribute
		// deployment.environment (renamed to deployment.environment.name
		// in later semconv versions). Backends accept both spellings
		// for now; we follow the package's current canonical helper.
		attrs = append(attrs, semconv.DeploymentEnvironment(env))
	}

	res, err := resource.New(ctx,
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithHost(),
		resource.WithAttributes(attrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	exp, err := newOTLPExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tracing: build OTLP exporter: %w", err)
	}

	// Clamp the configured sampler ratio AT Init so the legal
	// range is enforced at one site and an operator typo
	// (e.g. SAMPLER_ARG=10 meaning "10%") surfaces as a log line.
	ratio := cfg.SamplerRatio
	if ratio < 0 {
		slog.WarnContext(ctx, "tracing sampler ratio clamped",
			"configured", ratio, "applied", 0.0)
		ratio = 0
	}
	if ratio > MaxSamplerRatio {
		slog.WarnContext(ctx, "tracing sampler ratio clamped",
			"configured", ratio, "applied", MaxSamplerRatio)
		ratio = MaxSamplerRatio
	}
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithExportTimeout(exportTimeoutOrDefault(cfg.ExportTimeout)),
			sdktrace.WithMaxExportBatchSize(512),
		),
	)
	otel.SetTracerProvider(tp)

	return &Provider{
		tp:   tp,
		noop: false,
		shut: tp.Shutdown,
	}, nil
}

func exportTimeoutOrDefault(t time.Duration) time.Duration {
	if t <= 0 {
		return DefaultExportTimeout
	}
	return t
}

// newOTLPExporter constructs the OTLP/HTTP exporter from cfg. Split
// out so tests can stub it with a tracetest.SpanRecorder by
// constructing a Provider directly.
func newOTLPExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	opts := []otlptracehttp.Option{}
	endpoint, urlPath, err := splitOTLPEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	opts = append(opts, otlptracehttp.WithEndpoint(endpoint))
	if urlPath != "" {
		opts = append(opts, otlptracehttp.WithURLPath(urlPath))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		// Copy the map so a caller mutating the input after Init
		// can't change the exporter's auth headers mid-flight.
		hdr := make(map[string]string, len(cfg.Headers))
		for k, v := range cfg.Headers {
			hdr[k] = v
		}
		opts = append(opts, otlptracehttp.WithHeaders(hdr))
	}
	switch strings.ToLower(cfg.Compression) {
	case "gzip":
		opts = append(opts, otlptracehttp.WithCompression(otlptracehttp.GzipCompression))
	case "", "none":
		opts = append(opts, otlptracehttp.WithCompression(otlptracehttp.NoCompression))
	default:
		return nil, fmt.Errorf("tracing: unknown compression %q (want gzip, none)", cfg.Compression)
	}
	return otlptrace.New(ctx, otlptracehttp.NewClient(opts...))
}

// splitOTLPEndpoint normalises a user-supplied endpoint into the
// (host:port, urlPath) tuple the OTLP/HTTP client wants. It accepts:
//
//   - "host:4318"                 → ("host:4318", "")           default path applies
//   - "host"                      → ("host", "")
//   - "https://host:4318"         → ("host:4318", "")
//   - "https://host:4318/"        → ("host:4318", "")           trailing slash treated as no-path
//   - "http://host:4318/v1/traces"→ ("host:4318", "/v1/traces") Insecure inferred
//   - "host:4318/v1/traces"       → ("host:4318", "/v1/traces")
//
// The OTLP/HTTP exporter defaults to POST /v1/traces when WithURLPath
// is not set, which matches the convention every receiver implements.
// Operators pasting either form into OTEL_EXPORTER_OTLP_ENDPOINT
// expect it to "just work", so we normalise rather than rejecting.
//
// Trailing slash handling: an operator copy-pasting a base URL often
// includes a trailing "/" (e.g. "https://otlp.honeycomb.io:443/").
// Naively that produces urlPath="/" which OVERRIDES the SDK default
// "/v1/traces", causing every export request to 404 against the
// collector's root. We strip a sole trailing slash so the default
// path applies. Multi-segment paths ("/v1/traces/") still strip their
// trailing slash too (most receivers don't care, but a few are
// strict). Operators who genuinely want to POST to the collector's
// root (rare) can configure their collector with a non-trailing path.
//
// We deliberately do NOT auto-toggle WithInsecure off a "http://"
// prefix here — the Insecure field is the single source of truth for
// TLS state so an operator who pastes "http://" expecting a fallback
// gets a clear "Insecure must be true for plain HTTP" surface from
// the OTel client rather than a silent TLS upgrade attempt.
func splitOTLPEndpoint(raw string) (host string, urlPath string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", errors.New("tracing: empty endpoint")
	}
	if strings.HasPrefix(s, "https://") {
		s = strings.TrimPrefix(s, "https://")
	} else if strings.HasPrefix(s, "http://") {
		s = strings.TrimPrefix(s, "http://")
	}
	// Split on the first '/' so any path segment becomes the urlPath.
	if i := strings.IndexByte(s, '/'); i >= 0 {
		host = s[:i]
		urlPath = s[i:]
	} else {
		host = s
		urlPath = ""
	}
	// Strip a trailing slash from the path so an operator's
	// copy-pasted "https://host:port/" doesn't shadow the SDK
	// default "/v1/traces" with a useless "/" that 404s. After
	// trimming, a path that started as just "/" becomes "" (default
	// path applies); a path like "/v1/traces/" becomes "/v1/traces"
	// (the canonical form receivers expect).
	urlPath = strings.TrimRight(urlPath, "/")
	if host == "" {
		return "", "", fmt.Errorf("tracing: endpoint %q has no host", raw)
	}
	return host, urlPath, nil
}
