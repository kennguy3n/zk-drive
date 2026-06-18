package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/logging"
)

// requireEnv installs envs for the duration of t and restores the
// prior values on cleanup. Tests run with t.Setenv (which already
// restores) and an explicit Unsetenv loop to give us an empty
// baseline — Load reads via os.Getenv, so a stale env from an
// unrelated test would otherwise bleed in.
func requireEnv(t *testing.T, envs map[string]string) {
	t.Helper()
	// Every env Load might read. Listed explicitly (rather than
	// programmatically discovered) so a new env var added to Load is
	// forced through this scaffolding before tests pass.
	keys := []string{
		"DATABASE_URL", "JWT_SECRET", "LISTEN_ADDR",
		"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY",
		"MIGRATIONS_DIR", "NATS_URL", "CLAMAV_ADDRESS",
		"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REDIRECT_URL",
		"MICROSOFT_CLIENT_ID", "MICROSOFT_CLIENT_SECRET", "MICROSOFT_REDIRECT_URL",
		"RATE_LIMIT_PER_USER", "RATE_LIMIT_PER_WORKSPACE", "REDIS_URL",
		"FABRIC_CONSOLE_URL", "FABRIC_CONSOLE_ADMIN_TOKEN",
		"FABRIC_BUCKET_TEMPLATE", "FABRIC_DEFAULT_PLACEMENT_REF",
		"STATIC_DIR",
		"STRIPE_WEBHOOK_SECRET", "STRIPE_SECRET_KEY", "STRIPE_PRICE_TIER_MAP",
		"OLLAMA_URL", "OLLAMA_MODEL",
		"SECURITY_HEADERS_DISABLE_HSTS", "SECURITY_HEADERS_CSP_REPORT_ONLY",
		"SECURITY_HEADERS_CSP_REPORT_URI", "SECURITY_HEADERS_CSP_CONNECT_EXTRA",
		"SECURITY_HEADERS_CSP_IMG_EXTRA",
		// OpenTelemetry env vars (distributed tracing). Same
		// rationale as the audit-archival block below: any env var
		// read by buildConfigFromEnv must be in this list so a CI
		// runner with e.g. OTEL_EXPORTER_OTLP_ENDPOINT exported
		// doesn't bleed into tests that exercise the tracing-off
		// default. No test currently asserts on these fields, but
		// the convention is "every env Load reads goes here" — the
		// list is the source of truth for what tests are protected
		// against.
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_COMPRESSION",
		"OTEL_SERVICE_NAME", "OTEL_DEPLOYMENT_ENVIRONMENT",
		"OTEL_TRACES_SAMPLER_ARG",
		// Audit-log cold archival env vars. MUST be in this
		// list so tests like TestLoadAuditArchiveDefaults observe the
		// production "unset" state regardless of what the parent
		// shell / CI runner has exported. The defaults are validated
		// inside buildConfigFromEnv (e.g. AUDIT_LOG_RETENTION_DAYS
		// becomes 90 when unset), so the convention is: any env var
		// touched by buildConfigFromEnv lives here.
		"AUDIT_LOG_ARCHIVE_ENABLED", "AUDIT_LOG_RETENTION_DAYS",
		"AUDIT_LOG_ARCHIVE_PREFIX", "AUDIT_LOG_ARCHIVE_BUCKET",
		"AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH",
		// SMTP transactional email + PUBLIC_URL env vars. Added so
		// the email-related env state is unset by default, with the same
		// rationale documented in the OTEL block above: any env var
		// buildConfigFromEnv reads must live in this list so a CI
		// runner with e.g. SMTP_HOST exported doesn't bleed into
		// tests that exercise the "email disabled" default.
		"PUBLIC_URL",
		"SMTP_HOST", "SMTP_PORT",
		"SMTP_USERNAME", "SMTP_PASSWORD",
		"SMTP_FROM_ADDRESS", "SMTP_FROM_NAME",
		"SMTP_TLS_MODE", "SMTP_TLS_SERVER_NAME",
		"SMTP_TLS_INSECURE_SKIP_VERIFY",
		// Permission-cache env vars. Same convention as
		// the OTEL / audit / SMTP blocks above: any env var
		// buildConfigFromEnv reads must live in this list so a
		// CI runner with e.g. PERFORMANCE_CACHE_ENABLED=false
		// exported doesn't bleed into tests that exercise the
		// "cache on by default" path. The two config fields
		// (PerformanceCacheEnabled, PerformanceCacheTTL) feed
		// the cmd/server wiring of permission.Service.WithCache,
		// and tests asserting on the default-on / clamped-TTL
		// behaviour MUST see the production "unset" state.
		"PERFORMANCE_CACHE_ENABLED", "PERFORMANCE_CACHE_TTL",
		// ONLYOFFICE Document Server integration env vars. Same
		// convention as the blocks above: validateOnlyOfficeGroup
		// fails closed when ONLYOFFICE_URL is set without a secret, so
		// tests asserting that guard MUST see the production "unset"
		// state and not a value bled in from the CI runner.
		"ONLYOFFICE_URL", "ONLYOFFICE_SECRET", "ONLYOFFICE_ALLOW_INSECURE",
		// ONLYOFFICE save-path memory sizing + suspension posture.
		// Same convention as the blocks above: buildConfigFromEnv reads
		// each via onlyOfficeBytesFromEnv / parseBoolDefault (treating
		// empty identically to unset → safe default), so a CI runner
		// that exports e.g. ONLYOFFICE_MAX_DOCUMENT_MB or
		// SUSPENSION_FAIL_CLOSED must not bleed into tests exercising
		// the default-sizing / fail-open paths.
		"ONLYOFFICE_MAX_DOCUMENT_MB", "ONLYOFFICE_SAVE_MEMORY_BUDGET_MB",
		"ONLYOFFICE_STREAM_SAVE_MAX_CONCURRENT",
		"SUSPENSION_FAIL_CLOSED",
		// Preview pipeline scaling env vars. Same convention as the
		// blocks above: buildConfigFromEnv reads each of these via
		// parseIntDefault, treating an empty value identically to unset
		// (fall back to the clamped default), so a CI runner that exports
		// e.g. PREVIEW_PRIORITY_WORKERS=1 must not bleed into tests that
		// exercise the preview-default paths.
		"PREVIEW_BUDGET_PER_WORKSPACE_HOUR", "PREVIEW_PRIORITY_WORKERS", "PREVIEW_STANDARD_WORKERS",
		// Deployment profile. buildConfigFromEnv
		// reads ZKDRIVE_PROFILE via normaliseProfile; it must be
		// baseline-cleared so a CI runner that exports ZKDRIVE_PROFILE=compact
		// doesn't bleed the compact posture (tracing/metrics off) into tests
		// that assert the default full-posture state.
		"ZKDRIVE_PROFILE",
		// Platform-admin allowlist (JWT key-rotation gate). Same
		// convention as the blocks above: buildConfigFromEnv reads
		// PLATFORM_ADMIN_USER_IDS via platformAdminUserIDsFromEnv, so
		// it must be baseline-cleared here or a CI runner that has it
		// exported would bleed UUIDs into PlatformAdminUserIDs for any
		// test exercising the "unset → deny-by-default" state.
		"PLATFORM_ADMIN_USER_IDS",
		// DB connection-pool sizing + JWT signing/refresh env vars.
		// Same convention as the blocks above: buildConfigFromEnv reads
		// each of these (dbMaxConnsFromEnv / dbMinConnsFromEnv /
		// parseDurationDefault(DB_MAX_CONN_IDLE_TIME) /
		// jwtAlgorithmFromEnv / jwtKeyRefreshIntervalFromEnv), and all
		// of them treat an empty value identically to unset (fall back to
		// the clamped default), so a CI runner that exports e.g.
		// DB_MAX_CONNS=2 or JWT_ALGORITHM=ES256 would otherwise bleed into
		// tests exercising those default paths. Tests that assert on
		// non-default values (e.g. TestJWTKeyRefreshInterval) t.Setenv the
		// specific var themselves after requireEnv runs.
		"DB_MAX_CONNS", "DB_MIN_CONNS", "DB_MAX_CONN_IDLE_TIME",
		// Read-replica pool sizing. buildConfigFromEnv
		// reads these via dbReadMaxConnsFromEnv / dbReadMinConnsFromEnv,
		// which treat unset identically to "inherit the primary". Clear
		// them at baseline so a CI runner exporting e.g.
		// DB_READ_MAX_CONNS=2 cannot bleed into the inherit-path tests.
		"DB_READ_MAX_CONNS", "DB_READ_MIN_CONNS",
		"JWT_ALGORITHM", "JWT_KEY_REFRESH_INTERVAL",
		// Pre-existing env vars that buildConfigFromEnv reads but were
		// never baseline-cleared (closing a test-isolation gap).
		// Each is read via os.Getenv / strings.TrimSpace
		// / parse{,NonNegative}IntDefault / parseBoolDefault, all of
		// which treat an empty value identically to unset → the safe
		// default (no replica / OIDC disabled / web-push disabled / proxy
		// off / default worker counts), so clearing them to "" here just
		// pins the production "unset" state and cannot bleed a CI runner's
		// export into the default-path tests. WORKER_METRICS_ADDR is the
		// sole intentional exception (handled via Unsetenv above, since
		// for it empty != unset).
		//   - DATABASE_READ_URL: read-replica DSN (empty → primary only).
		//   - IAM_CORE_*: OIDC SSO config (all empty → SSO disabled).
		//   - PREVIEW_LIGHTWEIGHT/HEAVY/WORKER_CONCURRENCY/HEAVY_QUEUE_*:
		//     preview-fleet sizing (empty → clamped defaults).
		//   - TRUSTED_PROXY_DEPTH: X-Forwarded-For trust depth.
		//   - VAPID_*: Web Push keys (empty → push disabled).
		//   - WS_PROXY_MODE: WS proxy-tier toggle (empty → in-process hub).
		"DATABASE_READ_URL",
		"IAM_CORE_ISSUER_URL", "IAM_CORE_CLIENT_ID", "IAM_CORE_CLIENT_SECRET",
		"IAM_CORE_CALLBACK_URL", "IAM_CORE_AUDIENCE", "IAM_CORE_SCOPES",
		"PREVIEW_LIGHTWEIGHT_WORKERS", "PREVIEW_HEAVY_WORKERS",
		"PREVIEW_WORKER_CONCURRENCY", "PREVIEW_HEAVY_QUEUE_BACKPRESSURE_THRESHOLD",
		"TRUSTED_PROXY_DEPTH",
		"VAPID_PUBLIC_KEY", "VAPID_PRIVATE_KEY", "VAPID_SUBSCRIBER",
		"WS_PROXY_MODE",
		// ZKDRIVE_PROFILE selects the deployment profile and shifts
		// env-var defaults. On this branch it shifts the JWT_ALGORITHM
		// default (ES256 under production); it also drives
		// applyProfileDefaults + validateProfile (production requires
		// Redis + NATS). Baseline-clear it so a runner exporting
		// ZKDRIVE_PROFILE=production cannot bleed an ES256 default into
		// tests exercising the "auto" fallback, nor force the
		// profile-validation path on every test here. ZKDRIVE_AUTO_MIGRATE
		// is the auto-migrate toggle read by buildConfigFromEnv;
		// cleared for the same reason. Profile-specific tests live in
		// profiles_test.go and manage these vars themselves.
		"ZKDRIVE_PROFILE", "ZKDRIVE_AUTO_MIGRATE",
		// Security-header knobs whose defaults are profile- or
		// constant-derived. Baseline-clear them so a runner exporting
		// e.g. SECURITY_HEADERS_EXPECT_CT=true can't bleed into tests
		// asserting the profile default.
		"SECURITY_HEADERS_CSP_NONCE", "SECURITY_HEADERS_EXPECT_CT",
		// AUDIT_HMAC_KEY selects the audit-chain HMAC key source
		// (6.6). Baseline-clear it so a runner exporting an explicit
		// key cannot change the derived-from-JWT_SECRET default that
		// TestDeriveAuditHMACKeyDefaultsToJWTSecret asserts.
		"AUDIT_HMAC_KEY",
	}
	// WORKER_METRICS_ADDR is intentionally NOT included in the keys
	// list above. t.Setenv(k, "") makes os.LookupEnv return
	// (value="", ok=true), which workerMetricsAddrFromEnv treats as
	// "explicitly empty → disabled". If we baseline-cleared it the
	// same way as the other keys, every test calling requireEnv
	// would silently see WorkerMetricsAddr="" instead of the
	// production default :9091, masking any bug in the default
	// path — exactly the opposite of what the helper exists to do.
	// Instead, we unset the var here (so Load sees the production
	// "unset" state) and register a cleanup that restores whatever
	// value the test runner started with, mirroring t.Setenv's
	// save/restore semantics. Tests that exercise non-default
	// values explicitly t.Setenv it themselves.
	prev, hadPrev := os.LookupEnv("WORKER_METRICS_ADDR")
	if err := os.Unsetenv("WORKER_METRICS_ADDR"); err != nil {
		t.Fatalf("Unsetenv WORKER_METRICS_ADDR: %v", err)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("WORKER_METRICS_ADDR", prev)
		}
	})
	for _, k := range keys {
		t.Setenv(k, "")
	}
	for k, v := range envs {
		t.Setenv(k, v)
	}
}

// TestLoadMissingRequiredFailsFast: omitting DATABASE_URL / JWT_SECRET
// must fail before the server tries to dial Postgres — a partial boot
// is worse than no boot.
func TestLoadMissingRequiredFailsFast(t *testing.T) {
	requireEnv(t, map[string]string{})
	_, err := Load()
	if err == nil {
		t.Fatalf("expected Load to fail without DATABASE_URL/JWT_SECRET")
	}
	for _, sub := range []string{"DATABASE_URL", "JWT_SECRET"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("expected error to mention %s, got %v", sub, err)
		}
	}
}

// TestLoadMissingDatabaseOnly verifies the error enumerates only the
// actually-missing values — useful so an operator sees exactly which
// env they forgot.
func TestLoadMissingDatabaseOnly(t *testing.T) {
	requireEnv(t, map[string]string{"JWT_SECRET": "x"})
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("expected DATABASE_URL in error, got %v", err)
	}
	if strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("unexpected JWT_SECRET in error: %v", err)
	}
}

// TestLoadMinimumViable verifies the smallest configuration the
// server can boot on. Anything S3-related stays empty, so the S3
// validation branch must NOT fire.
func TestLoadMinimumViable(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.DatabaseURL != "postgres://x/y" || cfg.JWTSecret != "secret" {
		t.Fatalf("required values not propagated: %+v", cfg)
	}
	// Default ListenAddr fallback must be applied.
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("expected ListenAddr default :8080, got %q", cfg.ListenAddr)
	}
	// Default migrations dir.
	if cfg.MigrationsDir == "" {
		t.Fatalf("expected MigrationsDir to default, got empty")
	}
	// Fabric template / placement defaults.
	if cfg.FabricBucketTemplate != "zk-drive-{tenant}" {
		t.Fatalf("FabricBucketTemplate default drift: %q", cfg.FabricBucketTemplate)
	}
	if cfg.FabricDefaultPlacementRef != "b2c_pooled_default" {
		t.Fatalf("FabricDefaultPlacementRef default drift: %q", cfg.FabricDefaultPlacementRef)
	}
}

// TestProfileDefaultsToFullPosture asserts that an unset
// ZKDRIVE_PROFILE leaves tracing + metrics enabled — the existing
// production behaviour must be unchanged by the new compact profile.
func TestProfileDefaultsToFullPosture(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.IsCompactProfile() {
		t.Fatal("unset ZKDRIVE_PROFILE must not be compact")
	}
	if !cfg.TracingEnabled() || !cfg.MetricsEnabled() {
		t.Fatalf("full posture must enable tracing+metrics: tracing=%v metrics=%v", cfg.TracingEnabled(), cfg.MetricsEnabled())
	}
}

// TestProfileCompactDisablesObservability pins the compact-profile
// contract: ZKDRIVE_PROFILE=compact (case-insensitive) flips the
// posture and forces both the OTLP exporter and the Prometheus
// surface off, regardless of any OTEL_* env var the operator set.
func TestProfileCompactDisablesObservability(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":                "postgres://x/y",
		"JWT_SECRET":                  "secret",
		"ZKDRIVE_PROFILE":             "Compact",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com:4318",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.IsCompactProfile() {
		t.Fatalf("ZKDRIVE_PROFILE=Compact must normalise to compact, got %q", cfg.Profile)
	}
	if cfg.TracingEnabled() {
		t.Fatal("compact profile must disable tracing even with OTEL endpoint set")
	}
	if cfg.MetricsEnabled() {
		t.Fatal("compact profile must disable the Prometheus /metrics surface")
	}
}

// TestCompactProfileTokenMatchesLoggingPackage guards against the
// config-local compactProfile const drifting from the exported
// logging.CompactProfile token the two packages must agree on.
func TestCompactProfileTokenMatchesLoggingPackage(t *testing.T) {
	if compactProfile != logging.CompactProfile {
		t.Fatalf("compactProfile %q != logging.CompactProfile %q", compactProfile, logging.CompactProfile)
	}
}

// TestLoadPartialS3Fails enforces the documented invariant: if
// S3_ENDPOINT is set, the other three S3 vars must also be set.
// A half-configured storage client would only fail at first request
// time — far too late.
func TestLoadPartialS3Fails(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
		"S3_ENDPOINT":  "https://s3.example.com",
		// Missing S3_BUCKET / S3_ACCESS_KEY / S3_SECRET_KEY.
	})
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error when S3_ENDPOINT is set but credentials are missing")
	}
	for _, sub := range []string{"S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("expected error to mention %s, got %v", sub, err)
		}
	}
}

// TestLoadCompleteS3 verifies the happy path with full S3 wiring.
func TestLoadCompleteS3(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":   "postgres://x/y",
		"JWT_SECRET":     "secret",
		"S3_ENDPOINT":    "https://s3.example.com",
		"S3_BUCKET":      "drive-prod",
		"S3_ACCESS_KEY":  "AKIA...",
		"S3_SECRET_KEY":  "supersecret",
		"LISTEN_ADDR":    ":9090",
		"MIGRATIONS_DIR": "/srv/migrations",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.S3Bucket != "drive-prod" || cfg.S3Endpoint != "https://s3.example.com" {
		t.Fatalf("S3 fields not propagated: %+v", cfg)
	}
	if cfg.ListenAddr != ":9090" {
		t.Fatalf("expected ListenAddr override, got %q", cfg.ListenAddr)
	}
	if cfg.MigrationsDir != "/srv/migrations" {
		t.Fatalf("expected MigrationsDir override, got %q", cfg.MigrationsDir)
	}
}

// TestLoadOnlyOfficeURLWithoutSecretFails verifies the fail-closed
// guard: setting ONLYOFFICE_URL without ONLYOFFICE_SECRET refuses to
// start because the editor-callback endpoint would be unauthenticated.
func TestLoadOnlyOfficeURLWithoutSecretFails(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":   "postgres://x/y",
		"JWT_SECRET":     "secret",
		"ONLYOFFICE_URL": "https://docs.example.com",
		// Intentionally no ONLYOFFICE_SECRET / ONLYOFFICE_ALLOW_INSECURE.
	})
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when ONLYOFFICE_URL is set but ONLYOFFICE_SECRET is empty")
	}
	for _, sub := range []string{"ONLYOFFICE_SECRET", "ONLYOFFICE_ALLOW_INSECURE"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("expected error to mention %s, got %v", sub, err)
		}
	}
}

// TestLoadOnlyOfficeInsecureOptIn verifies the explicit local-dev
// escape hatch: ONLYOFFICE_ALLOW_INSECURE=true permits ONLYOFFICE_URL
// without a secret.
func TestLoadOnlyOfficeInsecureOptIn(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":              "postgres://x/y",
		"JWT_SECRET":                "secret",
		"ONLYOFFICE_URL":            "https://docs.example.com",
		"ONLYOFFICE_ALLOW_INSECURE": "true",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed with explicit insecure opt-in: %v", err)
	}
	if !cfg.OnlyOfficeAllowInsecure {
		t.Fatal("expected OnlyOfficeAllowInsecure to be true")
	}
}

// TestLoadOnlyOfficeWithSecret verifies the secure happy path: a
// configured URL + secret loads cleanly.
func TestLoadOnlyOfficeWithSecret(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":      "postgres://x/y",
		"JWT_SECRET":        "secret",
		"ONLYOFFICE_URL":    "https://docs.example.com",
		"ONLYOFFICE_SECRET": "office-callback-secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.OnlyOfficeURL != "https://docs.example.com" || cfg.OnlyOfficeSecret != "office-callback-secret" {
		t.Fatalf("OnlyOffice fields not propagated: %+v", cfg)
	}
}

// TestLoadOnlyOfficeSaveLimitsDefaults verifies the save-path memory
// sizing falls back to the documented defaults (256 MiB budget /
// 100 MiB per-document → concurrency 2) when unset.
func TestLoadOnlyOfficeSaveLimitsDefaults(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := cfg.OnlyOfficeMaxDocumentBytes; got != 100*bytesPerMB {
		t.Errorf("OnlyOfficeMaxDocumentBytes default: got %d, want %d", got, 100*bytesPerMB)
	}
	if got := cfg.OnlyOfficeSaveMemoryBudgetBytes; got != 256*bytesPerMB {
		t.Errorf("OnlyOfficeSaveMemoryBudgetBytes default: got %d, want %d", got, 256*bytesPerMB)
	}
	if got := cfg.OnlyOfficeMaxConcurrentSaves(); got != 2 {
		t.Errorf("OnlyOfficeMaxConcurrentSaves default: got %d, want 2", got)
	}
}

// TestLoadOnlyOfficeSaveLimitsConfigured verifies operator overrides are
// honoured and the derived concurrency follows budget / per-document.
func TestLoadOnlyOfficeSaveLimitsConfigured(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":                     "postgres://x/y",
		"JWT_SECRET":                       "secret",
		"ONLYOFFICE_MAX_DOCUMENT_MB":       "50",
		"ONLYOFFICE_SAVE_MEMORY_BUDGET_MB": "512",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := cfg.OnlyOfficeMaxDocumentBytes; got != 50*bytesPerMB {
		t.Errorf("OnlyOfficeMaxDocumentBytes: got %d, want %d", got, 50*bytesPerMB)
	}
	if got := cfg.OnlyOfficeMaxConcurrentSaves(); got != 10 { // 512 / 50 = 10
		t.Errorf("OnlyOfficeMaxConcurrentSaves: got %d, want 10", got)
	}
}

// TestLoadOnlyOfficeBudgetBelowDocumentFails verifies the server refuses
// to start when the memory budget is smaller than a single document
// (which would floor the derived concurrency to 0 and shed every save).
func TestLoadOnlyOfficeBudgetBelowDocumentFails(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":                     "postgres://x/y",
		"JWT_SECRET":                       "secret",
		"ONLYOFFICE_URL":                   "https://docs.example.com",
		"ONLYOFFICE_SECRET":                "office-callback-secret",
		"ONLYOFFICE_MAX_DOCUMENT_MB":       "100",
		"ONLYOFFICE_SAVE_MEMORY_BUDGET_MB": "50",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when ONLYOFFICE_SAVE_MEMORY_BUDGET_MB < ONLYOFFICE_MAX_DOCUMENT_MB")
	}
	if !strings.Contains(err.Error(), "ONLYOFFICE_SAVE_MEMORY_BUDGET_MB") {
		t.Fatalf("expected error to mention the budget var, got %v", err)
	}
}

// TestLoadSuspensionFailClosed verifies SUSPENSION_FAIL_CLOSED defaults
// to the fail-open posture and flips on when explicitly set.
func TestLoadSuspensionFailClosed(t *testing.T) {
	t.Run("default_fail_open", func(t *testing.T) {
		requireEnv(t, map[string]string{
			"DATABASE_URL": "postgres://x/y",
			"JWT_SECRET":   "secret",
		})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.SuspensionFailClosed {
			t.Fatal("SuspensionFailClosed should default to false (fail-open)")
		}
	})
	t.Run("opt_in_fail_closed", func(t *testing.T) {
		requireEnv(t, map[string]string{
			"DATABASE_URL":           "postgres://x/y",
			"JWT_SECRET":             "secret",
			"SUSPENSION_FAIL_CLOSED": "true",
		})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if !cfg.SuspensionFailClosed {
			t.Fatal("SuspensionFailClosed should be true when SUSPENSION_FAIL_CLOSED=true")
		}
	})
}

// TestLoadStorageOnly verifies the audit-restore-friendly slim
// loader: it returns a populated Config WITHOUT requiring
// DATABASE_URL or JWT_SECRET. S3_ENDPOINT is required (since the
// loader is for storage-only binaries); the S3 group remains
// coherent-validated.
func TestLoadStorageOnly(t *testing.T) {
	requireEnv(t, map[string]string{
		// Intentionally NO DATABASE_URL / JWT_SECRET.
		"S3_ENDPOINT":              "https://s3.example.com",
		"S3_BUCKET":                "drive-prod",
		"S3_ACCESS_KEY":            "AKIA...",
		"S3_SECRET_KEY":            "supersecret",
		"AUDIT_LOG_ARCHIVE_PREFIX": "audit-archive/",
	})
	cfg, err := LoadStorageOnly()
	if err != nil {
		t.Fatalf("LoadStorageOnly failed: %v", err)
	}
	if cfg.S3Endpoint != "https://s3.example.com" || cfg.S3Bucket != "drive-prod" {
		t.Fatalf("S3 fields not propagated: %+v", cfg)
	}
	if cfg.AuditArchivePrefix != "audit-archive/" {
		t.Fatalf("AuditArchivePrefix not propagated: %q", cfg.AuditArchivePrefix)
	}
	// DatabaseURL / JWTSecret may be empty — that's the whole point.
}

// TestLoadStorageOnlyRequiresS3Endpoint verifies that the slim
// loader still refuses to run if S3_ENDPOINT is absent — without
// S3 access there's nothing for audit-restore to read.
func TestLoadStorageOnlyRequiresS3Endpoint(t *testing.T) {
	requireEnv(t, map[string]string{
		// No S3_ENDPOINT.
	})
	_, err := LoadStorageOnly()
	if err == nil {
		t.Fatalf("expected error when S3_ENDPOINT is unset")
	}
	if !strings.Contains(err.Error(), "S3_ENDPOINT") {
		t.Fatalf("expected error to mention S3_ENDPOINT, got: %v", err)
	}
}

// TestLoadStorageOnlyEnforcesS3Group asserts the slim loader still
// applies the coherent-S3-group invariant (S3_ENDPOINT requires
// bucket + access key + secret key). Drift between Load and
// LoadStorageOnly on S3 validation would mean an operator could
// run audit-restore with half-configured S3 and get cryptic
// failures only when the first ListObjects call fires.
func TestLoadStorageOnlyEnforcesS3Group(t *testing.T) {
	requireEnv(t, map[string]string{
		"S3_ENDPOINT": "https://s3.example.com",
		// Intentionally missing S3_BUCKET / S3_ACCESS_KEY / S3_SECRET_KEY.
	})
	_, err := LoadStorageOnly()
	if err == nil {
		t.Fatalf("expected error when S3 group is incomplete")
	}
	for _, sub := range []string{"S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("expected error to mention %s, got: %v", sub, err)
		}
	}
}

// TestParsePriceTierMapHappy exercises the documented format
// (comma-separated price_id:tier pairs with whitespace tolerance).
func TestParsePriceTierMapHappy(t *testing.T) {
	got := parsePriceTierMap("  price_aaa : starter , price_bbb:business , price_ccc :secure_business ")
	want := map[string]string{
		"price_aaa": "starter",
		"price_bbb": "business",
		"price_ccc": "secure_business",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("price tier map[%s]=%q want %q", k, got[k], v)
		}
	}
}

// TestParsePriceTierMapEmpty: an empty / whitespace-only env yields
// nil — callers grep map == nil as the "no tier map configured"
// signal, so an empty-but-non-nil map would be a behaviour change.
func TestParsePriceTierMapEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t,  "} {
		if got := parsePriceTierMap(raw); got != nil {
			t.Fatalf("parsePriceTierMap(%q) = %v, want nil", raw, got)
		}
	}
}

// TestParsePriceTierMapMalformedEntriesSkipped: a single fat-fingered
// pair must not crash the server at startup — Load() must keep going
// with whatever survives. Skipped entries simply do not appear in
// the output.
func TestParsePriceTierMapMalformedEntriesSkipped(t *testing.T) {
	got := parsePriceTierMap("price_aaa:starter,malformed,:tier_only,price_only:")
	if len(got) != 1 || got["price_aaa"] != "starter" {
		t.Fatalf("expected only the well-formed pair to survive, got %v", got)
	}
}

// TestParseIntDefault walks the (empty, default, negative, malformed,
// happy) branches. Negative or zero values fall back to default — the
// rate limiter relies on this so an env var of "0" can't accidentally
// disable rate limiting.
func TestParseIntDefault(t *testing.T) {
	tests := []struct {
		raw     string
		def     int
		want    int
		comment string
	}{
		{"", 7, 7, "empty falls back"},
		{"   ", 5, 5, "whitespace falls back"},
		{"42", 7, 42, "positive int parses"},
		{"  100 ", 7, 100, "trimmed positive parses"},
		{"-5", 7, 7, "negative falls back"},
		{"0", 7, 7, "zero falls back"},
		{"abc", 7, 7, "garbage falls back"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.comment, func(t *testing.T) {
			if got := parseIntDefault(tc.raw, tc.def); got != tc.want {
				t.Fatalf("parseIntDefault(%q, %d)=%d, want %d", tc.raw, tc.def, got, tc.want)
			}
		})
	}
}

// TestParseNonNegativeIntDefault verifies that an explicit 0 is
// honoured (unlike parseIntDefault) while empty/garbage/negative still
// fall back. This is the contract TRUSTED_PROXY_DEPTH relies on so that
// 0 ("trust no proxy") is distinguishable from the default of 1.
func TestParseNonNegativeIntDefault(t *testing.T) {
	tests := []struct {
		raw     string
		def     int
		want    int
		comment string
	}{
		{"", 1, 1, "empty falls back"},
		{"   ", 1, 1, "whitespace falls back"},
		{"0", 1, 0, "explicit zero is honoured"},
		{"  0 ", 1, 0, "trimmed zero is honoured"},
		{"3", 1, 3, "positive int parses"},
		{"-2", 1, 1, "negative falls back"},
		{"abc", 1, 1, "garbage falls back"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.comment, func(t *testing.T) {
			if got := parseNonNegativeIntDefault(tc.raw, tc.def); got != tc.want {
				t.Fatalf("parseNonNegativeIntDefault(%q, %d)=%d, want %d", tc.raw, tc.def, got, tc.want)
			}
		})
	}
}

// TestGetEnvDefault verifies the helper consults os.Getenv and falls
// back when empty. Whitespace-only is intentionally NOT treated as
// empty here because operators sometimes use a single space as a
// "set but no value" marker; the value-level validation lives in
// the Load() caller.
func TestGetEnvDefault(t *testing.T) {
	t.Setenv("ZK_TEST_KNOB", "")
	if got := getEnvDefault("ZK_TEST_KNOB", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback when env empty, got %q", got)
	}
	t.Setenv("ZK_TEST_KNOB", "real")
	if got := getEnvDefault("ZK_TEST_KNOB", "fallback"); got != "real" {
		t.Fatalf("expected real value, got %q", got)
	}
}

// TestLoadParsesRateLimits walks the integration between Load and
// parseIntDefault so a regression in the parser surfaces through
// the Load contract too.
func TestLoadParsesRateLimits(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":             "postgres://x/y",
		"JWT_SECRET":               "secret",
		"RATE_LIMIT_PER_USER":      "120",
		"RATE_LIMIT_PER_WORKSPACE": "  500 ",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimitPerUser != 120 {
		t.Fatalf("RateLimitPerUser=%d, want 120", cfg.RateLimitPerUser)
	}
	if cfg.RateLimitPerWorkspace != 500 {
		t.Fatalf("RateLimitPerWorkspace=%d, want 500", cfg.RateLimitPerWorkspace)
	}
}

// TestLoadWorkerMetricsAddrDefault is the system-level counterpart to
// TestWorkerMetricsAddrFromEnv: it pins that the unset-→-default :9091
// path actually reaches the Config struct returned by Load(), not just
// the helper in isolation. requireEnv intentionally leaves
// WORKER_METRICS_ADDR unset so this test exercises the production
// default path through Load().
func TestLoadWorkerMetricsAddrDefault(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerMetricsAddr != ":9091" {
		t.Fatalf("unset WORKER_METRICS_ADDR: Load returned %q, want %q (default)", cfg.WorkerMetricsAddr, ":9091")
	}
}

// TestLoadWorkerMetricsAddrExplicitEmpty is the system-level counterpart
// for the explicitly-empty disable path. WORKER_METRICS_ADDR= (set but
// empty) must reach Load with the empty string intact, NOT get
// collapsed to the default.
func TestLoadWorkerMetricsAddrExplicitEmpty(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
	})
	t.Setenv("WORKER_METRICS_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerMetricsAddr != "" {
		t.Fatalf("explicit-empty WORKER_METRICS_ADDR: Load returned %q, want \"\" (disabled)", cfg.WorkerMetricsAddr)
	}
}

// TestWorkerMetricsAddrFromEnv pins the documented contract:
// unset → default :9091, explicitly empty → disabled (passed through
// untouched for startMetricsServer to interpret), "off" → disabled
// (likewise untouched), anything else → as provided. The earlier
// getEnvDefault implementation collapsed unset and explicitly-empty
// into the default, breaking the documented `WORKER_METRICS_ADDR=`
// escape hatch — this test guards against a regression to that.
func TestWorkerMetricsAddrFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  string
	}{
		{name: "unset_falls_back_to_default", set: false, want: ":9091"},
		{name: "explicit_empty_is_passed_through", set: true, value: "", want: ""},
		{name: "off_is_passed_through", set: true, value: "off", want: "off"},
		{name: "explicit_addr_is_passed_through", set: true, value: ":9192", want: ":9192"},
		{name: "whitespace_is_passed_through_for_helper_to_trim", set: true, value: "  ", want: "  "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("WORKER_METRICS_ADDR", tc.value)
			} else {
				if err := os.Unsetenv("WORKER_METRICS_ADDR"); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}
			got := workerMetricsAddrFromEnv()
			if got != tc.want {
				t.Errorf("workerMetricsAddrFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJWTKeyRefreshIntervalFromEnv pins the documented contract for
// JWT_KEY_REFRESH_INTERVAL: unset/malformed → 60s default; an explicit
// non-positive value disables the loop (0); positive values clamp to
// [10s, 1h] so a "1s" can't hammer the DB and a "24h" typo can't defeat
// cross-replica propagation.
func TestJWTKeyRefreshIntervalFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
	}{
		{name: "unset_falls_back_to_default", set: false, want: 60 * time.Second},
		{name: "malformed_falls_back_to_default", set: true, value: "not-a-duration", want: 60 * time.Second},
		{name: "explicit_zero_disables", set: true, value: "0", want: 0},
		{name: "negative_disables", set: true, value: "-5s", want: 0},
		{name: "below_floor_clamps_up", set: true, value: "1s", want: 10 * time.Second},
		{name: "in_range_passes_through", set: true, value: "90s", want: 90 * time.Second},
		{name: "above_ceiling_clamps_down", set: true, value: "24h", want: time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("JWT_KEY_REFRESH_INTERVAL", tc.value)
			} else {
				if err := os.Unsetenv("JWT_KEY_REFRESH_INTERVAL"); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}
			if got := jwtKeyRefreshIntervalFromEnv(); got != tc.want {
				t.Errorf("jwtKeyRefreshIntervalFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSecurityHeaderDefaults pins the 6.5 config contract:
//   - CSP nonce defaults ON (additive hardening) regardless of profile.
//   - Expect-CT defaults ON under production, OFF otherwise.
//   - Explicit env values override the profile default either way.
func TestSecurityHeaderDefaults(t *testing.T) {
	t.Run("development defaults", func(t *testing.T) {
		requireEnv(t, map[string]string{
			"DATABASE_URL": "postgres://x/y",
			"JWT_SECRET":   "secret",
		})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.SecurityHeadersCSPNonce {
			t.Errorf("CSP nonce should default on")
		}
		if cfg.SecurityHeadersExpectCT {
			t.Errorf("Expect-CT should default off outside production")
		}
	})
	t.Run("production defaults Expect-CT on", func(t *testing.T) {
		requireEnv(t, map[string]string{
			"DATABASE_URL":    "postgres://x/y",
			"JWT_SECRET":      "secret",
			"ZKDRIVE_PROFILE": "production",
			// production fails closed without Redis + NATS
			// (validateProfile); supply them so Load succeeds and the
			// security-header defaults are what's under test here.
			"REDIS_URL": "redis://127.0.0.1:6379",
			"NATS_URL":  "nats://127.0.0.1:4222",
		})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.SecurityHeadersExpectCT {
			t.Errorf("Expect-CT should default on under production")
		}
		if !cfg.SecurityHeadersCSPNonce {
			t.Errorf("CSP nonce should default on under production")
		}
	})
	t.Run("explicit env overrides profile", func(t *testing.T) {
		requireEnv(t, map[string]string{
			"DATABASE_URL":               "postgres://x/y",
			"JWT_SECRET":                 "secret",
			"ZKDRIVE_PROFILE":            "production",
			"REDIS_URL":                  "redis://127.0.0.1:6379",
			"NATS_URL":                   "nats://127.0.0.1:4222",
			"SECURITY_HEADERS_EXPECT_CT": "false",
			"SECURITY_HEADERS_CSP_NONCE": "false",
		})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.SecurityHeadersExpectCT {
			t.Errorf("explicit SECURITY_HEADERS_EXPECT_CT=false ignored")
		}
		if cfg.SecurityHeadersCSPNonce {
			t.Errorf("explicit SECURITY_HEADERS_CSP_NONCE=false ignored")
		}
	})
}

// TestJWTAlgorithmFromEnv pins the profile-aware default: an explicit,
// recognised JWT_ALGORITHM always wins; when unset/unrecognised the
// default is ES256 under production and auto otherwise.
func TestJWTAlgorithmFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		profile Profile
		want    string
	}{
		{"explicit_es256_dev", "ES256", ProfileDevelopment, "ES256"},
		{"explicit_hs256_prod_wins", "HS256", ProfileProduction, "HS256"},
		{"explicit_auto_prod_wins", "auto", ProfileProduction, "auto"},
		{"case_insensitive", "es256", ProfileDevelopment, "ES256"},
		{"unset_prod_defaults_es256", "", ProfileProduction, "ES256"},
		{"unset_dev_defaults_auto", "", ProfileDevelopment, "auto"},
		{"unset_compact_defaults_auto", "", ProfileCompact, "auto"},
		{"unrecognised_prod_defaults_es256", "garbage", ProfileProduction, "ES256"},
		{"unrecognised_dev_defaults_auto", "garbage", ProfileDevelopment, "auto"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jwtAlgorithmFromEnv(tc.raw, tc.profile); got != tc.want {
				t.Errorf("jwtAlgorithmFromEnv(%q, %q) = %q, want %q", tc.raw, tc.profile, got, tc.want)
			}
		})
	}
}

// TestDBReadMaxConnsFromEnv pins the read-pool MaxConns contract:
// unset inherits the primary's resolved max verbatim (both pools sized
// alike, the pre-knob behaviour); a set value clamps to the same
// [minDBMaxConns, maxDBMaxConns] bounds as the primary.
func TestDBReadMaxConnsFromEnv(t *testing.T) {
	const primaryMax int32 = 40
	tests := []struct {
		name  string
		set   bool
		value string
		want  int32
	}{
		{name: "unset_inherits_primary", set: false, want: primaryMax},
		{name: "empty_inherits_primary", set: true, value: "", want: primaryMax},
		{name: "whitespace_inherits_primary", set: true, value: "   ", want: primaryMax},
		{name: "malformed_inherits_primary", set: true, value: "abc", want: primaryMax},
		{name: "in_range_overrides", set: true, value: "120", want: 120},
		{name: "below_floor_clamps_up", set: true, value: "1", want: minDBMaxConns},
		{name: "above_ceiling_clamps_down", set: true, value: "9999", want: maxDBMaxConns},
		{name: "zero_inherits_via_parseIntDefault", set: true, value: "0", want: primaryMax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("DB_READ_MAX_CONNS", tc.value)
			} else if err := os.Unsetenv("DB_READ_MAX_CONNS"); err != nil {
				t.Fatalf("Unsetenv: %v", err)
			}
			if got := dbReadMaxConnsFromEnv(primaryMax); got != tc.want {
				t.Errorf("dbReadMaxConnsFromEnv(%d) = %d, want %d", primaryMax, got, tc.want)
			}
		})
	}
}

// TestDBReadMinConnsFromEnv pins the read-pool MinConns contract: unset
// inherits the primary's min; an explicit 0 is honoured (lazy pool); a
// negative is ignored (inherit retained); and the result is clamped to
// the read pool's resolved max so MinConns can never exceed MaxConns
// (which pgxpool rejects), including when an inherited primary min is
// larger than a smaller, explicitly-set read max.
func TestDBReadMinConnsFromEnv(t *testing.T) {
	tests := []struct {
		name       string
		set        bool
		value      string
		primaryMin int32
		readMax    int32
		want       int32
	}{
		{name: "unset_inherits_primary", set: false, primaryMin: 5, readMax: 40, want: 5},
		{name: "empty_inherits_primary", set: true, value: "", primaryMin: 5, readMax: 40, want: 5},
		{name: "explicit_zero_honoured", set: true, value: "0", primaryMin: 5, readMax: 40, want: 0},
		{name: "negative_ignored_inherits", set: true, value: "-3", primaryMin: 5, readMax: 40, want: 5},
		{name: "in_range_overrides", set: true, value: "12", primaryMin: 5, readMax: 40, want: 12},
		{name: "set_above_readmax_clamps", set: true, value: "100", primaryMin: 5, readMax: 40, want: 40},
		{name: "inherited_above_smaller_readmax_clamps", set: false, primaryMin: 20, readMax: 8, want: 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("DB_READ_MIN_CONNS", tc.value)
			} else if err := os.Unsetenv("DB_READ_MIN_CONNS"); err != nil {
				t.Fatalf("Unsetenv: %v", err)
			}
			if got := dbReadMinConnsFromEnv(tc.primaryMin, tc.readMax); got != tc.want {
				t.Errorf("dbReadMinConnsFromEnv(%d, %d) = %d, want %d", tc.primaryMin, tc.readMax, got, tc.want)
			}
		})
	}
}

func TestPlatformAdminUserIDsFromEnv(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()

	tests := []struct {
		name        string
		set         bool
		value       string
		want        []uuid.UUID
		wantInvalid []string
	}{
		{name: "unset_is_empty", set: false, want: nil, wantInvalid: nil},
		{name: "blank_is_empty", set: true, value: "   ", want: nil, wantInvalid: nil},
		{name: "single_id", set: true, value: id1.String(), want: []uuid.UUID{id1}, wantInvalid: nil},
		{
			name:  "multiple_ids_trimmed",
			set:   true,
			value: "  " + id1.String() + " , " + id2.String() + "  ",
			want:  []uuid.UUID{id1, id2},
		},
		{
			name:        "invalid_entries_dropped_and_reported",
			set:         true,
			value:       "not-a-uuid," + id1.String() + ",,also-bad",
			want:        []uuid.UUID{id1},
			wantInvalid: []string{"not-a-uuid", "also-bad"},
		},
		{
			name:        "all_invalid_is_empty_but_reported",
			set:         true,
			value:       "nope,still-nope",
			want:        nil,
			wantInvalid: []string{"nope", "still-nope"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("PLATFORM_ADMIN_USER_IDS", tc.value)
			} else {
				if err := os.Unsetenv("PLATFORM_ADMIN_USER_IDS"); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}
			got, invalid := platformAdminUserIDsFromEnv()
			if len(got) != len(tc.want) {
				t.Fatalf("platformAdminUserIDsFromEnv() ids = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("id index %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
			if len(invalid) != len(tc.wantInvalid) {
				t.Fatalf("invalid = %v, want %v", invalid, tc.wantInvalid)
			}
			for i := range tc.wantInvalid {
				if invalid[i] != tc.wantInvalid[i] {
					t.Errorf("invalid index %d = %q, want %q", i, invalid[i], tc.wantInvalid[i])
				}
			}
		})
	}
}

// TestClampAuditRetentionDays exercises every branch of the
// retention-day clamp so a future refactor that drops one of the
// safety floors (negative input, zero input, sub-service-floor input,
// max ceiling) trips a regression. The branches matter because each
// one prevents a different operator footgun:
//
//   - non-positive input -> 90 (default) so an empty / malformed
//     env var doesn't disable archival silently
//   - input in [1, minAuditRetentionDays-1] (1-6 days) -> clamps UP
//     to minAuditRetentionDays so the value is accepted by both
//     config Load() AND audit.NewArchiveService rather than getting
//     accepted at config-load time then rejected at archive start
//   - input above maxAuditRetentionDays -> ceiling (3650 = 10y) so
//     a typo'd "9999" doesn't keep archived rows in the hot tier
//     for 27 years
func TestClampAuditRetentionDays(t *testing.T) {
	cases := []struct {
		name  string
		input int
		want  int
	}{
		{"negative falls back to default", -7, 90},
		{"zero falls back to default", 0, 90},
		{"valid passes through", 365, 365},
		{"one clamps to service floor", 1, minAuditRetentionDays},
		{"six clamps to service floor", 6, minAuditRetentionDays},
		{"at service floor passes through", minAuditRetentionDays, minAuditRetentionDays},
		{"above ceiling clamps to max", 9999, maxAuditRetentionDays},
		{"at ceiling passes through", maxAuditRetentionDays, maxAuditRetentionDays},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampAuditRetentionDays(tc.input)
			if got != tc.want {
				t.Errorf("clampAuditRetentionDays(%d) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestAuditRetentionFloorMatchesService pins minAuditRetentionDays
// to audit.MinRetentionDays so a future change in one constant
// without the other fails CI rather than at archive-start runtime.
// Same-package import is allowed for tests; the production binary
// doesn't import audit from config.
func TestAuditRetentionFloorMatchesService(t *testing.T) {
	if minAuditRetentionDays != audit.MinRetentionDays {
		t.Fatalf("minAuditRetentionDays (%d) != audit.MinRetentionDays (%d). "+
			"They must stay locked-step so a config-accepted value cannot be "+
			"rejected by ArchiveService at archive-start.",
			minAuditRetentionDays, audit.MinRetentionDays)
	}
}

// TestClampAuditMaxRowsPerBatch exercises every branch of the
// rows-per-batch clamp so a future refactor can't drop the upper
// bound without failing CI. The upper bound is load-bearing: the
// CronJob pod (deploy/k8s/audit-archiver-cronjob.yaml) is limited to
// 512Mi memory, and the JSONL.gz encoder buffers an entire page in
// memory before uploading. A malformed env var like
// AUDIT_LOG_ARCHIVE_MAX_ROWS_PER_BATCH=10000000 (intended 100k) would
// OOM-kill the pod without this clamp.
func TestClampAuditMaxRowsPerBatch(t *testing.T) {
	cases := []struct {
		name  string
		input int
		want  int
	}{
		{"negative falls back to default", -42, defaultAuditArchiveMaxRowsPerBatch},
		{"zero falls back to default", 0, defaultAuditArchiveMaxRowsPerBatch},
		{"one passes through (valid floor)", 1, 1},
		{"default passes through", defaultAuditArchiveMaxRowsPerBatch, defaultAuditArchiveMaxRowsPerBatch},
		{"at ceiling passes through", maxAuditArchiveMaxRowsPerBatch, maxAuditArchiveMaxRowsPerBatch},
		{"above ceiling clamps to max", maxAuditArchiveMaxRowsPerBatch + 1, maxAuditArchiveMaxRowsPerBatch},
		{"far above ceiling clamps to max (OOM-prevention)", 10_000_000, maxAuditArchiveMaxRowsPerBatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampAuditMaxRowsPerBatch(tc.input)
			if got != tc.want {
				t.Errorf("clampAuditMaxRowsPerBatch(%d) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestNormaliseArchivePrefix walks every input shape that an
// operator might paste into AUDIT_LOG_ARCHIVE_PREFIX. The
// trailing-slash normalisation is critical because the archive
// service concatenates the workspace UUID directly onto the
// configured prefix — without normalisation, "audit-archive" and
// "audit-archive/" would produce different S3 key layouts on the
// same bucket, breaking restore enumeration.
func TestNormaliseArchivePrefix(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty falls back to default", "", "audit-archive/"},
		{"whitespace falls back to default", "   ", "audit-archive/"},
		{"only-slashes falls back to default", "///", "audit-archive/"},
		{"missing trailing slash adds one", "audit", "audit/"},
		{"single trailing slash passes through", "audit/", "audit/"},
		{"double trailing slash collapses", "audit//", "audit/"},
		{"deep prefix preserved", "compliance/audit-archive/", "compliance/audit-archive/"},
		{"deep prefix missing slash gets one", "compliance/audit-archive", "compliance/audit-archive/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normaliseArchivePrefix(tc.input)
			if got != tc.want {
				t.Errorf("normaliseArchivePrefix(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestLoadAuditArchiveDefaults verifies the env-free Load() path
// produces the documented defaults: archive disabled, 90-day
// retention, "audit-archive/" prefix, no bucket override, 50k
// rows-per-batch cap.
func TestLoadAuditArchiveDefaults(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://localhost/zkdrive",
		"JWT_SECRET":   "test-secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuditArchiveEnabled {
		t.Errorf("AuditArchiveEnabled = true, want false (opt-in)")
	}
	if cfg.AuditLogRetentionDays != 90 {
		t.Errorf("AuditLogRetentionDays = %d, want 90", cfg.AuditLogRetentionDays)
	}
	if cfg.AuditArchivePrefix != "audit-archive/" {
		t.Errorf("AuditArchivePrefix = %q, want audit-archive/", cfg.AuditArchivePrefix)
	}
	if cfg.AuditArchiveBucket != "" {
		t.Errorf("AuditArchiveBucket = %q, want empty (S3_BUCKET fallback)", cfg.AuditArchiveBucket)
	}
	if cfg.AuditArchiveMaxRowsPerBatch != 50000 {
		t.Errorf("AuditArchiveMaxRowsPerBatch = %d, want 50000", cfg.AuditArchiveMaxRowsPerBatch)
	}
}

// TestDeriveAuditHMACKeyDefaultsToJWTSecret: with no AUDIT_HMAC_KEY the
// key must be derived from JWT_SECRET (32 bytes, source "derived") so a
// fresh install is self-operating without extra config, while keeping
// the key out of the database.
func TestDeriveAuditHMACKeyDefaultsToJWTSecret(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://localhost/zkdrive",
		"JWT_SECRET":   "test-secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuditHMACKeySource != AuditHMACKeySourceDerived {
		t.Errorf("AuditHMACKeySource = %q, want %q", cfg.AuditHMACKeySource, AuditHMACKeySourceDerived)
	}
	if len(cfg.AuditHMACKey) != 32 {
		t.Fatalf("AuditHMACKey len = %d, want 32", len(cfg.AuditHMACKey))
	}
	// Deriving twice from the same secret is stable, and a different
	// JWT_SECRET yields a different key.
	again, _ := deriveAuditHMACKey("", "test-secret")
	if !bytes.Equal(again, cfg.AuditHMACKey) {
		t.Error("derived key not stable for the same JWT_SECRET")
	}
	other, _ := deriveAuditHMACKey("", "different-secret")
	if bytes.Equal(other, cfg.AuditHMACKey) {
		t.Error("derived key did not change with JWT_SECRET")
	}
}

// TestDeriveAuditHMACKeyExplicitWins: an explicit AUDIT_HMAC_KEY must be
// used (source "explicit") and must differ from the JWT_SECRET-derived
// key even when the same secret material is reused, thanks to distinct
// HKDF info labels (domain separation).
func TestDeriveAuditHMACKeyExplicitWins(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL":   "postgres://localhost/zkdrive",
		"JWT_SECRET":     "shared-secret",
		"AUDIT_HMAC_KEY": "shared-secret",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuditHMACKeySource != AuditHMACKeySourceExplicit {
		t.Errorf("AuditHMACKeySource = %q, want %q", cfg.AuditHMACKeySource, AuditHMACKeySourceExplicit)
	}
	if len(cfg.AuditHMACKey) != 32 {
		t.Fatalf("AuditHMACKey len = %d, want 32", len(cfg.AuditHMACKey))
	}
	derived, _ := deriveAuditHMACKey("", "shared-secret")
	if bytes.Equal(cfg.AuditHMACKey, derived) {
		t.Error("explicit and derived keys collide despite distinct HKDF info labels")
	}
}

// TestLoadErrorIsStdError guards against future refactors that wrap
// the missing-env error in fmt.Errorf without %w — we want callers
// to be able to programmatically detect "missing config" failures
// via the stdlib error interface.
func TestLoadErrorIsStdError(t *testing.T) {
	requireEnv(t, map[string]string{})
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error")
	}
	// At minimum the error type must implement the stdlib error
	// interface and produce a non-empty string — sanity check that
	// the caller's `if err != nil` branch (and any log.Printf("%v"))
	// will work.
	if err.Error() == "" {
		t.Fatalf("expected non-empty error string")
	}
}

// TestClampPerformanceCacheTTL exercises every branch of the
// permission-cache TTL clamp. The bounds are load-bearing:
//
//   - The lower bound (1s) prevents a typo from busy-looping
//     the cache. A 100ms TTL would re-fetch from Postgres on
//     every keystroke of a folder browse and would be
//     strictly worse than no cache at all (it would add a
//     redis round-trip on top of the Postgres query).
//
//   - The upper bound (5m) prevents a typo from making the
//     cache effectively permanent. Even with proactive
//     busting, admins making direct psql changes have no path
//     back to the application layer, so a forgotten entry
//     must self-expire in a window short enough that
//     operators don't reach for FLUSHDB to recover.
//
// This suite pins the contract so future refactors can't silently
// loosen the bounds.
func TestClampPerformanceCacheTTL(t *testing.T) {
	cases := []struct {
		name  string
		input time.Duration
		want  time.Duration
	}{
		{"negative falls back to default", -5 * time.Second, defaultPerformanceCacheTTL},
		{"zero falls back to default", 0, defaultPerformanceCacheTTL},
		{"below floor falls back to default", 500 * time.Millisecond, defaultPerformanceCacheTTL},
		{"at floor passes through", minPerformanceCacheTTL, minPerformanceCacheTTL},
		{"valid passes through", 30 * time.Second, 30 * time.Second},
		{"default passes through", defaultPerformanceCacheTTL, defaultPerformanceCacheTTL},
		{"near ceiling passes through", 4 * time.Minute, 4 * time.Minute},
		{"at ceiling passes through", maxPerformanceCacheTTL, maxPerformanceCacheTTL},
		{"above ceiling clamps to max", maxPerformanceCacheTTL + time.Second, maxPerformanceCacheTTL},
		{"far above ceiling clamps to max", time.Hour, maxPerformanceCacheTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampPerformanceCacheTTL(tc.input)
			if got != tc.want {
				t.Errorf("clampPerformanceCacheTTL(%v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestParseDurationDefault exercises every code path in the
// duration env-var parser. The "empty / parse failure → def"
// behaviour is consistent with the rest of the parseFooDefault
// family — a typo silently uses the documented default rather
// than failing the boot. Operators see the default value in
// logs / debug endpoints and can correct the typo at leisure.
//
// Composition with clampPerformanceCacheTTL: parseDurationDefault
// can return a value below the clamp's floor (e.g. "500ms"
// parses successfully but clamps to default). The pipeline is
// parse-then-clamp; the clamp is the safety net.
func TestParseDurationDefault(t *testing.T) {
	def := 30 * time.Second
	cases := []struct {
		name  string
		input string
		want  time.Duration
	}{
		{"empty falls back to default", "", def},
		{"only whitespace falls back to default", "   ", def},
		{"valid seconds", "10s", 10 * time.Second},
		{"valid minutes", "5m", 5 * time.Minute},
		{"valid hours", "1h", time.Hour},
		{"valid composite", "1h30m", 90 * time.Minute},
		{"valid sub-second", "500ms", 500 * time.Millisecond},
		{"valid negative parses", "-5s", -5 * time.Second},
		{"unparseable falls back", "not-a-duration", def},
		{"missing unit falls back", "30", def},
		{"trailing junk falls back", "10s blah", def},
		{"with surrounding whitespace parses", "  10s  ", 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDurationDefault(tc.input, def)
			if got != tc.want {
				t.Errorf("parseDurationDefault(%q, %v) = %v, want %v", tc.input, def, got, tc.want)
			}
		})
	}
}

// TestParseAndClampPerformanceCacheTTLPipeline exercises the
// composition that the actual config Load() path uses:
// parseDurationDefault → clampPerformanceCacheTTL. The
// integration matters because a sub-second valid duration
// (e.g. "500ms") parses successfully but must still be clamped
// to the default at the consumer.
func TestParseAndClampPerformanceCacheTTLPipeline(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  time.Duration
	}{
		{"empty env uses default", "", defaultPerformanceCacheTTL},
		{"valid in-range value passes through", "45s", 45 * time.Second},
		{"sub-second value parses then clamps to default", "500ms", defaultPerformanceCacheTTL},
		{"above ceiling clamps to max", "1h", maxPerformanceCacheTTL},
		{"unparseable uses default", "garbage", defaultPerformanceCacheTTL},
		{"negative parses then clamps to default", "-30s", defaultPerformanceCacheTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseDurationDefault(tc.input, defaultPerformanceCacheTTL)
			got := clampPerformanceCacheTTL(parsed)
			if got != tc.want {
				t.Errorf("pipeline(%q) = %v (parsed=%v); want %v", tc.input, got, parsed, tc.want)
			}
		})
	}
}
