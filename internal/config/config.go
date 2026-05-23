package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config holds runtime configuration for the zk-drive server and worker
// binaries. All values are sourced from environment variables so deployments
// can inject them uniformly.
type Config struct {
	DatabaseURL   string
	JWTSecret     string
	ListenAddr    string
	S3Endpoint    string
	S3Bucket      string
	S3AccessKey   string
	S3SecretKey   string
	MigrationsDir string
	NATSURL       string
	ClamAVAddress string

	// SSO — optional, Business-tier feature. When the client id is
	// empty for a provider, the corresponding /api/auth/oauth/{provider}
	// routes return 501 Not Implemented so the rest of the server still
	// boots without credentials.
	GoogleClientID        string
	GoogleClientSecret    string
	GoogleRedirectURL     string
	MicrosoftClientID     string
	MicrosoftClientSecret string
	MicrosoftRedirectURL  string

	// Rate limiting — applied per (workspace_id, user_id) via an
	// in-memory token bucket. Values <= 0 fall back to the defaults
	// declared alongside the middleware so misconfigured env vars do
	// not accidentally disable rate limiting entirely.
	RateLimitPerUser      int
	RateLimitPerWorkspace int

	// RedisURL switches the rate limiter and session store from
	// in-memory state to a Redis-backed implementation so limits and
	// session revocation work across replicas. When empty, the
	// in-memory implementations are used (single-replica behaviour).
	RedisURL string

	// FabricConsoleURL is the base URL of the zk-object-fabric console
	// API (e.g. "https://console.fabric.example.com"). When empty,
	// signup falls back to the static S3_* env vars and per-workspace
	// tenant provisioning is disabled.
	FabricConsoleURL string
	// FabricConsoleAdminToken is sent as a bearer token on console
	// admin endpoints (placement read / write). The signup endpoint is
	// public and does not require it.
	FabricConsoleAdminToken string
	// FabricBucketTemplate names the bucket created per tenant. The
	// literal string "{tenant}" is replaced with the new tenant ID.
	// When empty, defaults to "zk-drive-{tenant}".
	FabricBucketTemplate string
	// FabricDefaultPlacementRef is the placement_policy_ref recorded
	// on freshly provisioned workspaces. Defaults to
	// "b2c_pooled_default" to mirror the migration default.
	FabricDefaultPlacementRef string

	// StaticDir, when non-empty, makes the server serve a single-page
	// app (typically the Vite-built `frontend/dist`) on every request
	// that doesn't match an `/api` route or `/healthz`. Missing files
	// fall back to `index.html` so client-side routes (`/drive`,
	// `/login`, ...) work on a hard refresh. Leaving it empty keeps
	// the server API-only, which is the production deployment shape.
	StaticDir string

	// Stripe — billing webhook integration. When
	// StripeWebhookSecret is empty the /api/webhooks/stripe route is
	// still mounted but every request is rejected with 400 because
	// signatures cannot be verified. StripeSecretKey is currently
	// only used for outbound API calls (e.g. retrieving expanded
	// objects) and is reserved for future server-initiated flows.
	StripeWebhookSecret string
	StripeSecretKey     string
	// StripePriceTierMap maps Stripe price IDs to billing tier names
	// (`starter`, `business`, `secure_business`). Parsed from the
	// STRIPE_PRICE_TIER_MAP env var as comma-separated
	// `price_id:tier` pairs, e.g.
	// `price_123:starter,price_456:business`. Empty values fall
	// through to the price object's metadata.tier field.
	StripePriceTierMap map[string]string

	// Local on-device LLM. When OLLAMA_URL is set the AI summary
	// service routes through the daemon at that address (default
	// 127.0.0.1:11434, the standard Ollama port) using the model
	// named in OLLAMA_MODEL (default qwen2.5:1.5b). When unset, the
	// summary service stays on the deterministic rule-based
	// scaffold — there is no external-API fallback by design, the
	// product never sends file content to a third-party LLM.
	OllamaURL   string
	OllamaModel string

	// Browser security headers (CSP / HSTS / etc.) emitted on
	// every response by api/middleware.SecurityHeaders. The
	// middleware ships a safe default policy; these env vars
	// are knobs operators reach for during rollout or when
	// integrating the storage gateway origin.
	//
	// SecurityHeadersDisableHSTS skips Strict-Transport-Security.
	// Useful for local HTTP development; should remain false in
	// production where TLS terminates at the ingress.
	SecurityHeadersDisableHSTS bool
	// SecurityHeadersCSPReportOnly emits the policy under
	// Content-Security-Policy-Report-Only instead of enforcing.
	// Use during initial rollout to confirm the report stream
	// is clean before flipping the switch.
	SecurityHeadersCSPReportOnly bool
	// SecurityHeadersCSPReportURI is appended as `report-uri`
	// to the CSP value. Browsers POST violation reports there.
	SecurityHeadersCSPReportURI string
	// SecurityHeadersCSPConnectExtra is a comma-separated list
	// of additional origins to allow in `connect-src` (on top of
	// the default `'self'`). The default deliberately does NOT
	// include bare `wss:` / `ws:` scheme sources — `'self'`
	// already covers same-origin WebSocket upgrades, and bare
	// `wss:` would allow XSS exfiltration to any host. The
	// fabric storage gateway URL goes here (required so
	// presigned-URL uploads / downloads land); a cross-origin
	// WebSocket gateway URL (if any) goes here too.
	SecurityHeadersCSPConnectExtra []string
	// SecurityHeadersCSPImgExtra is a comma-separated list of
	// additional origins to allow in `img-src` (on top of
	// `'self' data: blob:`). Thumbnails / previews served from
	// the storage gateway origin go here.
	SecurityHeadersCSPImgExtra []string

	// WorkerMetricsAddr is the listen address for the worker
	// binary's dedicated /metrics HTTP server. Default ":9091"
	// (one port above the server's default :8080 so a single
	// host can run both without conflict). Set to "off" or the
	// empty string to disable the metrics server entirely —
	// useful for sidecar-only deployments where the operator
	// uses a different collection path. "Unset" and "explicitly
	// empty" are distinct: an unset env var falls back to the
	// default :9091, while WORKER_METRICS_ADDR= (explicit empty)
	// is the documented disable. The server binary exposes
	// /metrics on its main port (ListenAddr) and does NOT
	// consult this field. The reconciler binary is short-
	// lived and does not export metrics; see README.
	WorkerMetricsAddr string

	// PublicURL is the canonical externally-reachable base URL of
	// the frontend (e.g. "https://drive.example.com"). The email
	// service uses it to compose invite-accept links; when empty,
	// transactional email is forcibly disabled (links would point
	// at an unrouted host). Trailing slashes are stripped at
	// consumption time so operators can paste either form.
	PublicURL string

	// SMTP — transactional email transport. The email service
	// boots into a NoopClient when SMTPHost is empty (logged at
	// startup), so dev environments don't fail to come up. In
	// production, leaving SMTPHost unset means guest-invite
	// emails are silently no-ops; the operator README documents
	// the trade-off and the in-app notification path that still
	// fires for known users.
	SMTPHost                  string
	SMTPPort                  int
	SMTPUsername              string
	SMTPPassword              string
	SMTPFromAddress           string
	SMTPFromName              string
	SMTPTLSMode               string // "starttls" (default), "implicit", "none"
	SMTPTLSServerName         string
	SMTPTLSInsecureSkipVerify bool

	// OpenTelemetry tracing. When OTELExporterOTLPEndpoint is
	// empty, the tracing subsystem installs a no-op tracer so the
	// server boots without an exporter wired up. Setting the
	// endpoint flips on the OTLP/HTTP exporter; the rest of the
	// fields tune the exporter and sampler around that. All values
	// are surfaced through tracing.LogStartup at server boot so an
	// operator can confirm the configuration without grep'ing the
	// pod's env.
	//
	// Endpoints follow the upstream OTEL_EXPORTER_OTLP_ENDPOINT
	// convention: a base URL like "https://otlp.example.com:4318"
	// (the package appends "/v1/traces"), or a full URL like
	// "https://otlp.example.com:4318/v1/traces" — both forms
	// work, the exporter normalises them at init time.
	OTELExporterOTLPEndpoint string
	// Headers is a comma-separated list of header_name=header_value
	// pairs, matching the W3C-style OTEL_EXPORTER_OTLP_HEADERS
	// env var (e.g. "x-honeycomb-team=KEY,x-tenant=acme"). Used
	// to authenticate against managed backends. Parsed at load
	// time so a malformed entry surfaces as a startup warning
	// rather than a silent silent-drop at the first trace export.
	OTELExporterOTLPHeaders map[string]string
	// Insecure disables TLS for the exporter. Only set for a
	// local collector running over plain HTTP — production should
	// always terminate TLS.
	OTELExporterOTLPInsecure bool
	// Compression: "gzip" (default) or "none". Matches the
	// upstream OTEL_EXPORTER_OTLP_COMPRESSION semantics.
	OTELExporterOTLPCompression string
	// ServiceName is the service.name resource attribute every
	// span carries. Defaults to "zk-drive" so spans from the
	// server and worker binaries land under the same logical
	// service in the backend.
	OTELServiceName string
	// DeploymentEnvironment is the deployment.environment
	// resource attribute (e.g. "production", "staging", "dev").
	// Defaults to empty so dev environments don't accidentally
	// tag themselves "production".
	OTELDeploymentEnvironment string
	// SamplerRatio is the parent-based ratio sampler value in
	// [0,1]. Defaults to 0.1 (10%) for production. Set to 1.0
	// for dev / debug; set to 0.0 to disable sampling entirely
	// (preferred to setting endpoint="" if you want the SDK
	// initialised but no traces exported).
	OTELSamplerRatio float64

	// Audit-log cold archival (WS-23). When AuditArchiveEnabled
	// is false (the default), the audit-archiver binary refuses
	// to run — operators must explicitly opt in so a fresh
	// install can't accidentally start deleting audit history
	// before the operator has confirmed the S3 archive prefix
	// is writable and the retention window matches their
	// compliance posture.
	AuditArchiveEnabled bool
	// AuditLogRetentionDays is the hot-tier retention window
	// (rows older than now() - this many days are eligible for
	// archival). 90 days is the typical SOC2 Type II hot tier;
	// values <= 0 disable archival even when AuditArchiveEnabled
	// is true so a misconfigured 0 doesn't silently archive
	// everything. The maximum allowed value (10 years) is
	// clamped at config-load time to catch a stray multiplier
	// typo.
	AuditLogRetentionDays int
	// AuditArchivePrefix is the S3 key prefix every archive
	// object is written under. Defaults to "audit-archive/".
	// Operators wanting to use a dedicated bucket can set
	// AuditArchiveBucket; when empty the configured S3_BUCKET
	// is reused so a single-bucket deployment is the default.
	AuditArchivePrefix string
	// AuditArchiveBucket overrides S3_BUCKET for archive
	// writes. Empty means use S3_BUCKET. Useful when the
	// operator wants compliance-grade lifecycle policies (e.g.
	// Glacier transition, object-lock retention) on the
	// archive bucket independent of the live file store.
	AuditArchiveBucket string
	// AuditArchiveMaxRowsPerBatch caps the number of audit_log
	// rows packed into a single (workspace, month) JSONL.gz
	// upload. Defaults to 50000 — at ~512 bytes per audit
	// entry that's roughly a 25 MB uncompressed / 5 MB
	// compressed object, comfortably below S3's 5 GB single-
	// upload limit and small enough that a single PUT round-
	// trip stays under the 60s exporter context budget. When
	// a (workspace, month) batch exceeds this, the archiver
	// writes multiple JSONL.gz objects with distinct UUID
	// suffixes — the restore tool joins them transparently.
	AuditArchiveMaxRowsPerBatch int
}

// Load reads configuration from environment variables and returns a populated
// Config. It returns an error if any required variable is missing or empty.
//
// The S3 group (S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY) is
// optional so the server can boot for metadata-only development without a
// running zk-object-fabric gateway. However, if S3_ENDPOINT is set, the
// bucket, access key, and secret key must also be set — a half-configured
// storage client would only fail at request time.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		JWTSecret:             os.Getenv("JWT_SECRET"),
		ListenAddr:            getEnvDefault("LISTEN_ADDR", ":8080"),
		S3Endpoint:            os.Getenv("S3_ENDPOINT"),
		S3Bucket:              os.Getenv("S3_BUCKET"),
		S3AccessKey:           os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:           os.Getenv("S3_SECRET_KEY"),
		MigrationsDir:         getEnvDefault("MIGRATIONS_DIR", "migrations"),
		NATSURL:               os.Getenv("NATS_URL"),
		ClamAVAddress:         os.Getenv("CLAMAV_ADDRESS"),
		GoogleClientID:        os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:    os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:     os.Getenv("GOOGLE_REDIRECT_URL"),
		MicrosoftClientID:     os.Getenv("MICROSOFT_CLIENT_ID"),
		MicrosoftClientSecret: os.Getenv("MICROSOFT_CLIENT_SECRET"),
		MicrosoftRedirectURL:  os.Getenv("MICROSOFT_REDIRECT_URL"),
		RateLimitPerUser:        parseIntDefault(os.Getenv("RATE_LIMIT_PER_USER"), 0),
		RateLimitPerWorkspace:   parseIntDefault(os.Getenv("RATE_LIMIT_PER_WORKSPACE"), 0),
		RedisURL:                os.Getenv("REDIS_URL"),
		FabricConsoleURL:        os.Getenv("FABRIC_CONSOLE_URL"),
		FabricConsoleAdminToken: os.Getenv("FABRIC_CONSOLE_ADMIN_TOKEN"),
		FabricBucketTemplate:    getEnvDefault("FABRIC_BUCKET_TEMPLATE", "zk-drive-{tenant}"),
		FabricDefaultPlacementRef: getEnvDefault("FABRIC_DEFAULT_PLACEMENT_REF", "b2c_pooled_default"),
		StaticDir:                 os.Getenv("STATIC_DIR"),
		StripeWebhookSecret:       os.Getenv("STRIPE_WEBHOOK_SECRET"),
		StripeSecretKey:           os.Getenv("STRIPE_SECRET_KEY"),
		StripePriceTierMap:        parsePriceTierMap(os.Getenv("STRIPE_PRICE_TIER_MAP")),
		OllamaURL:                 os.Getenv("OLLAMA_URL"),
		OllamaModel:               os.Getenv("OLLAMA_MODEL"),

		SecurityHeadersDisableHSTS:     parseBoolDefault(os.Getenv("SECURITY_HEADERS_DISABLE_HSTS"), false),
		SecurityHeadersCSPReportOnly:   parseBoolDefault(os.Getenv("SECURITY_HEADERS_CSP_REPORT_ONLY"), false),
		SecurityHeadersCSPReportURI:    os.Getenv("SECURITY_HEADERS_CSP_REPORT_URI"),
		SecurityHeadersCSPConnectExtra: parseCSVList(os.Getenv("SECURITY_HEADERS_CSP_CONNECT_EXTRA")),
		SecurityHeadersCSPImgExtra:     parseCSVList(os.Getenv("SECURITY_HEADERS_CSP_IMG_EXTRA")),

		// WorkerMetricsAddr uses LookupEnv (not getEnvDefault) so the
		// documented contract holds: unset → default :9091; explicitly
		// empty (WORKER_METRICS_ADDR=) → disabled. getEnvDefault would
		// collapse both into the default, breaking the operator's
		// expectation that `=` disables the server.
		WorkerMetricsAddr: workerMetricsAddrFromEnv(),

		PublicURL: strings.TrimSpace(os.Getenv("PUBLIC_URL")),

		SMTPHost:                  strings.TrimSpace(os.Getenv("SMTP_HOST")),
		SMTPPort:                  parseIntDefault(os.Getenv("SMTP_PORT"), 587),
		SMTPUsername:              os.Getenv("SMTP_USERNAME"),
		SMTPPassword:              os.Getenv("SMTP_PASSWORD"),
		SMTPFromAddress:           strings.TrimSpace(os.Getenv("SMTP_FROM_ADDRESS")),
		SMTPFromName:              os.Getenv("SMTP_FROM_NAME"),
		SMTPTLSMode:               getEnvDefault("SMTP_TLS_MODE", "starttls"),
		SMTPTLSServerName:         os.Getenv("SMTP_TLS_SERVER_NAME"),
		SMTPTLSInsecureSkipVerify: parseBoolDefault(os.Getenv("SMTP_TLS_INSECURE_SKIP_VERIFY"), false),

		OTELExporterOTLPEndpoint:    strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		OTELExporterOTLPHeaders:     parseOTELHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")),
		OTELExporterOTLPInsecure:    parseBoolDefault(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), false),
		OTELExporterOTLPCompression: strings.ToLower(strings.TrimSpace(getEnvDefault("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip"))),
		OTELServiceName:             getEnvDefault("OTEL_SERVICE_NAME", "zk-drive"),
		OTELDeploymentEnvironment:   os.Getenv("OTEL_DEPLOYMENT_ENVIRONMENT"),
		OTELSamplerRatio:            parseFloatDefault(os.Getenv("OTEL_TRACES_SAMPLER_ARG"), 0.1),

		AuditArchiveEnabled:         parseBoolDefault(os.Getenv("AUDIT_LOG_ARCHIVE_ENABLED"), false),
		AuditLogRetentionDays:       clampAuditRetentionDays(parseIntDefault(os.Getenv("AUDIT_LOG_RETENTION_DAYS"), 90)),
		AuditArchivePrefix:          normaliseArchivePrefix(getEnvDefault("AUDIT_LOG_ARCHIVE_PREFIX", "audit-archive/")),
		AuditArchiveBucket:          strings.TrimSpace(os.Getenv("AUDIT_LOG_ARCHIVE_BUCKET")),
		AuditArchiveMaxRowsPerBatch: parseIntDefault(os.Getenv("AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH"), 50000),
	}

	var missing []string
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if strings.TrimSpace(cfg.JWTSecret) == "" {
		missing = append(missing, "JWT_SECRET")
	}
	if len(missing) > 0 {
		return nil, errors.New("missing required environment variables: " + strings.Join(missing, ", "))
	}

	if strings.TrimSpace(cfg.S3Endpoint) != "" {
		var missingS3 []string
		if strings.TrimSpace(cfg.S3Bucket) == "" {
			missingS3 = append(missingS3, "S3_BUCKET")
		}
		if strings.TrimSpace(cfg.S3AccessKey) == "" {
			missingS3 = append(missingS3, "S3_ACCESS_KEY")
		}
		if strings.TrimSpace(cfg.S3SecretKey) == "" {
			missingS3 = append(missingS3, "S3_SECRET_KEY")
		}
		if len(missingS3) > 0 {
			return nil, errors.New("S3_ENDPOINT is set but missing required variables: " + strings.Join(missingS3, ", "))
		}
	}
	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// workerMetricsAddrFromEnv distinguishes "unset" (fall back to the
// default :9091) from "explicitly empty" (disable the worker metrics
// server entirely). getEnvDefault collapses both into the default,
// which silently breaks the documented `WORKER_METRICS_ADDR=`
// escape hatch — operators who follow the README and unset/empty
// the variable to disable the server would still get :9091. Using
// LookupEnv preserves the distinction.
func workerMetricsAddrFromEnv() string {
	v, ok := os.LookupEnv("WORKER_METRICS_ADDR")
	if !ok {
		return ":9091"
	}
	return v
}

// parsePriceTierMap parses a comma-separated list of
// `price_id:tier` pairs into a lookup map. Whitespace around each
// pair and the colon is tolerated; malformed pairs are skipped so a
// fat-fingered env var doesn't crash the server at startup.
func parsePriceTierMap(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.Index(pair, ":")
		if idx <= 0 || idx == len(pair)-1 {
			continue
		}
		priceID := strings.TrimSpace(pair[:idx])
		tier := strings.TrimSpace(pair[idx+1:])
		if priceID == "" || tier == "" {
			continue
		}
		out[priceID] = tier
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maxAuditRetentionDays caps AUDIT_LOG_RETENTION_DAYS at ten years
// (3650 days) so a typo like "9000000" (intended "90") doesn't
// degenerate into "archive nothing ever" and let the hot tier grow
// unboundedly with no operator-visible warning. Ten years exceeds
// every common compliance retention window (SOC2 = 1 year minimum,
// HIPAA = 6 years) and the clamp log makes the corrected value
// surface in startup logs.
const maxAuditRetentionDays = 3650

// minAuditRetentionDays mirrors internal/audit.MinRetentionDays (= 7)
// — the service-level floor that ArchiveService refuses to operate
// below. They MUST stay locked-step: a value that passes config but
// fails the service produces a confusing operator experience where
// the binary loads successfully and then aborts at archive start.
// internal/config/config_test.go pins the equality with a Go-level
// assertion so a future change in either constant fails CI.
const minAuditRetentionDays = 7

// clampAuditRetentionDays bounds AUDIT_LOG_RETENTION_DAYS at sensible
// limits. Returns the configured value when valid; otherwise:
//   - non-positive input clamps to 90 (the default — assume the
//     operator forgot to set it rather than intentionally set 0).
//   - input in the range [1, minAuditRetentionDays-1] (i.e. 1–6 days)
//     clamps UP to minAuditRetentionDays. Values that low almost
//     certainly indicate a typo and aggressively pruning legitimately-
//     recent audit history would compound the mistake. Clamping up
//     preserves audit history rather than deleting it.
//   - input > maxAuditRetentionDays clamps to maxAuditRetentionDays.
//
// The caller surfaces the clamped value at startup via the boot logs
// so an operator can confirm the effective setting.
func clampAuditRetentionDays(d int) int {
	if d <= 0 {
		return 90
	}
	if d < minAuditRetentionDays {
		return minAuditRetentionDays
	}
	if d > maxAuditRetentionDays {
		return maxAuditRetentionDays
	}
	return d
}

// normaliseArchivePrefix ensures the prefix ends with exactly one
// trailing slash so callers can concatenate the per-workspace key
// suffix without bookkeeping. Empty input falls back to the default
// "audit-archive/" — never an empty prefix, because writing to the
// bucket root would tangle archive objects with live file objects.
func normaliseArchivePrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "audit-archive/"
	}
	// Strip every trailing slash first, then re-add exactly one,
	// so "audit/", "audit//", and "audit///" all normalise to
	// "audit/".
	s = strings.TrimRight(s, "/")
	if s == "" {
		// Was just slashes — fall back to default rather than
		// returning the bucket root.
		return "audit-archive/"
	}
	return s + "/"
}

// parseBoolDefault parses the common boolean env-var values
// ("1", "true", "yes", "on") case-insensitively. Anything else
// (including the empty string) falls through to def, which
// keeps a typo from silently flipping a security knob.
func parseBoolDefault(s string, def bool) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// parseCSVList splits a comma-separated env-var value into a
// trimmed slice, dropping empty entries. Returns nil for empty
// input so the caller's omit-empty default kicks in.
func parseCSVList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseIntDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// parseFloatDefault parses a non-negative float env-var value. Empty
// strings, parse failures, and negative results fall back to def —
// the typical use is parsing a sampler ratio in [0,1], so silently
// clamping a typo back to the default is preferable to crashing.
// Values above 1.0 are passed through unchanged; the consumer
// (tracing.Init) clamps to the legal sampler range with its own
// startup warning so the limit is documented at one site.
func parseFloatDefault(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// parseOTELHeaders parses a W3C-style comma-separated list of
// `key=value` pairs into a header map. Matches the upstream
// OTEL_EXPORTER_OTLP_HEADERS conventions, including treating
// surrounding whitespace as ignorable. Returns nil for empty input
// so the consumer can distinguish "no headers configured" from
// "an empty map I should still iterate". Malformed pairs (no `=`,
// empty key, empty value) are skipped rather than crashing — the
// tracing package's startup log warns when the input was non-empty
// but the parsed result is empty so a typo doesn't silently strip
// auth headers from the exporter.
func parseOTELHeaders(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.Index(pair, "=")
		if idx <= 0 || idx == len(pair)-1 {
			continue
		}
		k := strings.TrimSpace(pair[:idx])
		v := strings.TrimSpace(pair[idx+1:])
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
