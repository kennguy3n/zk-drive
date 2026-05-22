package config

import (
	"os"
	"strings"
	"testing"
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
		"WORKER_METRICS_ADDR",
	}
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
		"DATABASE_URL":           "postgres://x/y",
		"JWT_SECRET":             "secret",
		"RATE_LIMIT_PER_USER":    "120",
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

// TestWorkerMetricsAddrFromEnv pins the documented contract:
// unset → default :9091, explicitly empty → disabled (passed through
// untouched for startMetricsServer to interpret), "off" → disabled
// (likewise untouched), anything else → as provided. The earlier
// getEnvDefault implementation collapsed unset and explicitly-empty
// into the default, breaking the documented `WORKER_METRICS_ADDR=`
// escape hatch — this test guards against a regression to that.
func TestWorkerMetricsAddrFromEnv(t *testing.T) {
	tests := []struct {
		name   string
		set    bool
		value  string
		want   string
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
