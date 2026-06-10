package config

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// cleanProfileEnv unsets ZKDRIVE_PROFILE and every profile-defaulted env
// var for the duration of t, restoring the prior values on cleanup.
//
// Unlike requireEnv (which sets each key to ""), this UNSETS the keys so
// os.LookupEnv reports them as absent. applyProfileDefaults only fills a
// key when LookupEnv says it is absent, so the "fills unset" behaviour is
// only observable from a genuinely-unset baseline — setting "" would be
// treated as an explicit operator choice and skipped.
func cleanProfileEnv(t *testing.T) {
	t.Helper()
	keys := append([]string{"ZKDRIVE_PROFILE", "REDIS_URL"}, profileEnvKeys...)
	for _, k := range keys {
		prev, had := os.LookupEnv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("Unsetenv %s: %v", k, err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, prev)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
}

// TestApplyProfileDefaultsCompactFillsUnset: the compact profile fills
// every one of its defaults when the env is otherwise empty.
func TestApplyProfileDefaultsCompactFillsUnset(t *testing.T) {
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "compact")

	p, err := applyProfileDefaults()
	if err != nil {
		t.Fatalf("applyProfileDefaults: %v", err)
	}
	if p != ProfileCompact {
		t.Fatalf("profile = %q, want compact", p)
	}
	for k, want := range profileEnvDefaults(ProfileCompact) {
		if got := os.Getenv(k); got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
}

// TestApplyProfileDefaultsExplicitWins: a value the operator set
// explicitly must NOT be overwritten by the profile default.
func TestApplyProfileDefaultsExplicitWins(t *testing.T) {
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "compact")
	t.Setenv("NATS_URL", "nats://ops-chosen:4222")
	t.Setenv("DB_MAX_CONNS", "42")

	if _, err := applyProfileDefaults(); err != nil {
		t.Fatalf("applyProfileDefaults: %v", err)
	}
	if got := os.Getenv("NATS_URL"); got != "nats://ops-chosen:4222" {
		t.Errorf("NATS_URL = %q, want operator value preserved", got)
	}
	if got := os.Getenv("DB_MAX_CONNS"); got != "42" {
		t.Errorf("DB_MAX_CONNS = %q, want operator value preserved", got)
	}
	// A default the operator did NOT set is still filled.
	if got := os.Getenv("DB_MIN_CONNS"); got != "1" {
		t.Errorf("DB_MIN_CONNS = %q, want compact default 1", got)
	}
}

// TestApplyProfileDefaultsEmptyExplicitWins: an explicitly-empty var
// (LookupEnv ok=true, value="") is an operator choice and must be left
// empty, not back-filled. This is the case requireEnv relies on.
func TestApplyProfileDefaultsEmptyExplicitWins(t *testing.T) {
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "compact")
	t.Setenv("NATS_URL", "")

	if _, err := applyProfileDefaults(); err != nil {
		t.Fatalf("applyProfileDefaults: %v", err)
	}
	if got, ok := os.LookupEnv("NATS_URL"); !ok || got != "" {
		t.Errorf("NATS_URL = (%q, ok=%v), want explicit empty preserved", got, ok)
	}
}

// TestApplyProfileDefaultsUnknownFailsClosed: a typo'd profile name is
// rejected rather than silently running with zero presets.
func TestApplyProfileDefaultsUnknownFailsClosed(t *testing.T) {
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "compactt")

	p, err := applyProfileDefaults()
	if err == nil {
		t.Fatalf("expected error for unknown profile")
	}
	if p != "" {
		t.Errorf("profile = %q, want empty on error", p)
	}
	// Error enumerates the valid values so an operator can self-correct.
	for _, name := range []string{"compact", "development", "production"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q should mention valid profile %q", err, name)
		}
	}
}

// TestApplyProfileDefaultsEmptyNoop: no ZKDRIVE_PROFILE → no profile, no
// env mutation (pre-profile behaviour preserved).
func TestApplyProfileDefaultsEmptyNoop(t *testing.T) {
	cleanProfileEnv(t)

	p, err := applyProfileDefaults()
	if err != nil {
		t.Fatalf("applyProfileDefaults: %v", err)
	}
	if p != "" {
		t.Errorf("profile = %q, want empty", p)
	}
	for _, k := range profileEnvKeys {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("env %s should remain unset with no profile", k)
		}
	}
}

// TestApplyProfileDefaultsCaseInsensitive: ZKDRIVE_PROFILE is parsed
// case-insensitively and trims surrounding whitespace.
func TestApplyProfileDefaultsCaseInsensitive(t *testing.T) {
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "  Compact  ")

	p, err := applyProfileDefaults()
	if err != nil {
		t.Fatalf("applyProfileDefaults: %v", err)
	}
	if p != ProfileCompact {
		t.Fatalf("profile = %q, want compact", p)
	}
}

// TestApplyProfileDefaultsProductionNoDefaults: production contributes
// no env defaults (it relies on operator-supplied Redis/NATS).
func TestApplyProfileDefaultsProductionNoDefaults(t *testing.T) {
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "production")

	p, err := applyProfileDefaults()
	if err != nil {
		t.Fatalf("applyProfileDefaults: %v", err)
	}
	if p != ProfileProduction {
		t.Fatalf("profile = %q, want production", p)
	}
	for _, k := range profileEnvKeys {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("env %s should remain unset for production profile", k)
		}
	}
}

// TestValidateProfileProductionRequiresRedisAndNATS: production fails
// closed when either Redis or NATS is missing, and the error names the
// specific missing var(s).
func TestValidateProfileProductionRequiresRedisAndNATS(t *testing.T) {
	tests := []struct {
		name      string
		redis     string
		nats      string
		wantErr   bool
		wantNames []string
	}{
		{name: "both missing", wantErr: true, wantNames: []string{"REDIS_URL", "NATS_URL"}},
		{name: "redis only", redis: "redis://r:6379", nats: "", wantErr: true, wantNames: []string{"NATS_URL"}},
		{name: "nats only", redis: "", nats: "nats://n:4222", wantErr: true, wantNames: []string{"REDIS_URL"}},
		{name: "both present", redis: "redis://r:6379", nats: "nats://n:4222", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Profile: string(ProfileProduction), RedisURL: tc.redis, NATSURL: tc.nats}
			err := validateProfile(cfg)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateProfile err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				for _, n := range tc.wantNames {
					if !strings.Contains(err.Error(), n) {
						t.Errorf("error %q should mention %q", err, n)
					}
				}
				// nats-only case must NOT also complain about REDIS_URL.
				if tc.name == "redis only" && strings.Contains(err.Error(), "REDIS_URL") {
					t.Errorf("error %q should not mention REDIS_URL", err)
				}
			}
		})
	}
}

// TestValidateProfileNonProductionNoop: compact / development / no
// profile impose no requirements even with Redis and NATS unset.
func TestValidateProfileNonProductionNoop(t *testing.T) {
	for _, p := range []string{string(ProfileCompact), string(ProfileDevelopment), ""} {
		cfg := &Config{Profile: p}
		if err := validateProfile(cfg); err != nil {
			t.Errorf("validateProfile(profile=%q) = %v, want nil", p, err)
		}
	}
}

// TestProfileEnvKeysInSyncWithDefaults: every key any profile defaults
// must be declared in profileEnvKeys (the auditable allowlist), and
// profileEnvKeys must not declare keys no profile actually sets. Keeps
// the two in lockstep so the allowlist can't silently drift.
func TestProfileEnvKeysInSyncWithDefaults(t *testing.T) {
	declared := map[string]bool{}
	for _, k := range profileEnvKeys {
		declared[k] = true
	}
	used := map[string]bool{}
	for _, p := range []Profile{ProfileCompact, ProfileProduction, ProfileDevelopment} {
		for k := range profileEnvDefaults(p) {
			used[k] = true
			if !declared[k] {
				t.Errorf("profile %q defaults %q which is not in profileEnvKeys", p, k)
			}
		}
	}
	for k := range declared {
		if !used[k] {
			t.Errorf("profileEnvKeys declares %q but no profile defaults it", k)
		}
	}
}

// TestProfileNamesSorted: profileNames is the source of valid values in
// error messages; it must be the full sorted set.
func TestProfileNamesSorted(t *testing.T) {
	got := profileNames()
	want := []string{"compact", "development", "production"}
	if !sort.StringsAreSorted(got) {
		t.Errorf("profileNames not sorted: %v", got)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("profileNames = %v, want %v", got, want)
	}
}

// TestLoadCompactProfileEndToEnd: Load with ZKDRIVE_PROFILE=compact and
// only the required vars resolves the compact defaults end-to-end —
// auto-migrate on, embedded-NATS URL, trimmed pool — proving the
// applyProfileDefaults → buildConfigFromEnv wiring works through Load.
func TestLoadCompactProfileEndToEnd(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
	})
	// requireEnv baselines the profile-default keys to "", which
	// applyProfileDefaults treats as explicit (and skips). Unset them so
	// the compact defaults are observable, then select the profile.
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "compact")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profile != "compact" {
		t.Errorf("Profile = %q, want compact", cfg.Profile)
	}
	if !cfg.AutoMigrate {
		t.Errorf("AutoMigrate = false, want true (compact auto-migrates)")
	}
	if cfg.NATSURL != "nats://127.0.0.1:4222" {
		t.Errorf("NATSURL = %q, want embedded-NATS default", cfg.NATSURL)
	}
}

// TestLoadProductionProfileFailsClosed: Load with ZKDRIVE_PROFILE=
// production but no Redis/NATS must fail (not boot a replica with unsafe
// in-memory fallbacks).
func TestLoadProductionProfileFailsClosed(t *testing.T) {
	requireEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"JWT_SECRET":   "secret",
	})
	cleanProfileEnv(t)
	t.Setenv("ZKDRIVE_PROFILE", "production")

	if _, err := Load(); err == nil {
		t.Fatalf("expected Load to fail closed for production without Redis/NATS")
	}
}
