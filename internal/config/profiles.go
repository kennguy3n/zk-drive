package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Profile is a named bundle of environment-variable defaults that
// collapses the 50+ individual knobs Config reads into a single
// deployment-shape selector. It is chosen with ZKDRIVE_PROFILE.
//
// The goal is NoOps for non-technical SME admins: a compact
// single-node deployment should need only the handful of vars that are
// genuinely site-specific (DATABASE_URL, JWT_SECRET, and the S3 group)
// and inherit everything else from the profile. An explicitly-set env
// var always wins over a profile default, so the profile only fills in
// blanks — it never overrides an operator's deliberate choice.
type Profile string

const (
	// ProfileCompact is the single-node SME shape: in-memory rate
	// limiter + session store (no Redis), embedded NATS, auto-migrate
	// on startup, ClamAV optional, ONLYOFFICE + AI disabled. Sized for
	// a deployment that runs the server and worker in one container
	// under ~512MB RAM (deploy/docker-compose.compact.yml).
	ProfileCompact Profile = "compact"

	// ProfileProduction is the horizontally-scaled multi-replica
	// shape: Redis and NATS are REQUIRED (validateProfile fails closed
	// if either is missing) because in-memory rate limiting / session
	// revocation and an embedded broker are not safe across replicas.
	// Migrations run out-of-band via the migrate Job, so auto-migrate
	// stays off.
	ProfileProduction Profile = "production"

	// ProfileDevelopment is the local-laptop shape: same in-memory
	// defaults as compact (no external dependencies required) but
	// without compact's resource clamps, so a developer's box is free
	// to use the full connection pool. No required-var validation.
	ProfileDevelopment Profile = "development"
)

// profileEnvKeys lists every env var a profile may default, so
// validateProfile can warn when an unknown profile name is given and
// so the set stays auditable in one place. Kept in sync with
// profileEnvDefaults by the profiles_test.go round-trip test.
var profileEnvKeys = []string{
	"NATS_URL",
	"ZKDRIVE_AUTO_MIGRATE",
	"DB_MAX_CONNS",
	"DB_MIN_CONNS",
	"PREVIEW_PRIORITY_WORKERS",
	"PREVIEW_STANDARD_WORKERS",
}

// profileEnvDefaults returns the env-var defaults a profile contributes.
// The map is applied only-if-unset (see applyProfileDefaults), so each
// entry is a fallback, never an override. A nil/empty map means "no
// defaults" (development deliberately adds none beyond the shared
// in-memory behaviour that already kicks in when the vars are empty).
func profileEnvDefaults(p Profile) map[string]string {
	switch p {
	case ProfileCompact:
		return map[string]string{
			// Embedded NATS (cmd/compact) listens on loopback:4222.
			// The server/worker children connect here.
			"NATS_URL": "nats://127.0.0.1:4222",
			// Compact owns its schema: no separate migrate Job exists
			// in the single-container shape, so the server applies
			// pending migrations under the advisory lock at startup.
			"ZKDRIVE_AUTO_MIGRATE": "true",
			// Small pool: one box, two in-container consumers
			// (server + worker), an embedded Postgres tuned for low
			// memory. 10 max keeps us well under a default
			// max_connections=100 even with both processes.
			"DB_MAX_CONNS": "10",
			"DB_MIN_CONNS": "1",
			// Trim the preview goroutine pools — a single-tenant box
			// does not need the 6/2 fan-out the shared fleet uses.
			"PREVIEW_PRIORITY_WORKERS": "2",
			"PREVIEW_STANDARD_WORKERS": "1",
		}
	case ProfileProduction, ProfileDevelopment:
		// Production relies on operator-supplied Redis/NATS and the
		// out-of-band migrate Job, so it contributes no defaults — it
		// only adds the validateProfile requirements. Development
		// inherits the zero-value in-memory behaviour with no clamps.
		return nil
	default:
		return nil
	}
}

// normaliseProfile parses ZKDRIVE_PROFILE case-insensitively. An empty
// value yields the empty Profile (no profile selected — every var is
// read directly, preserving the pre-profile behaviour). An unrecognised
// value is returned as-is so applyProfileDefaults can reject it with a
// clear error rather than silently ignoring a typo.
func normaliseProfile(s string) Profile {
	return Profile(strings.ToLower(strings.TrimSpace(s)))
}

// knownProfile reports whether p is one of the recognised profiles.
func knownProfile(p Profile) bool {
	switch p {
	case ProfileCompact, ProfileProduction, ProfileDevelopment:
		return true
	default:
		return false
	}
}

// applyProfileDefaults reads ZKDRIVE_PROFILE and, for a recognised
// profile, sets each of that profile's env-var defaults that is not
// already present in the environment. It returns the resolved profile.
//
// Contract:
//   - Unset / empty ZKDRIVE_PROFILE → no-op, returns "" (no profile).
//   - Unknown value → error (fail closed: a typo'd profile must not
//     silently run with zero presets and a surprising config).
//   - Explicit env vars always win: a key already present (even if set
//     to the empty string) is left untouched.
//
// It mutates process environment so the existing os.Getenv-based
// buildConfigFromEnv picks the values up without threading the profile
// through every field. Idempotent: re-running it makes no further
// changes once the defaults are in place.
func applyProfileDefaults() (Profile, error) {
	p := normaliseProfile(os.Getenv("ZKDRIVE_PROFILE"))
	if p == "" {
		return "", nil
	}
	if !knownProfile(p) {
		return "", fmt.Errorf("unknown ZKDRIVE_PROFILE %q: valid values are %s", p, strings.Join(profileNames(), ", "))
	}
	for k, v := range profileEnvDefaults(p) {
		if _, ok := os.LookupEnv(k); ok {
			continue
		}
		// os.Setenv only fails on an invalid (empty / NUL-containing)
		// key; profileEnvKeys is a fixed allowlist of valid names, so
		// this never errors in practice — surface it regardless rather
		// than swallowing it.
		if err := os.Setenv(k, v); err != nil {
			return p, fmt.Errorf("apply %s profile default %s: %w", p, k, err)
		}
	}
	return p, nil
}

// validateProfile enforces the production fail-closed requirements
// once the Config is built. Compact and development impose no
// requirements (they run dependency-free); production requires Redis
// and NATS because its in-memory fallbacks are unsafe across replicas.
func validateProfile(cfg *Config) error {
	if Profile(cfg.Profile) != ProfileProduction {
		return nil
	}
	var missing []string
	if strings.TrimSpace(cfg.RedisURL) == "" {
		missing = append(missing, "REDIS_URL")
	}
	if strings.TrimSpace(cfg.NATSURL) == "" {
		missing = append(missing, "NATS_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("ZKDRIVE_PROFILE=production requires %s: in-memory rate limiting / session revocation and an embedded broker are not safe across replicas", strings.Join(missing, ", "))
	}
	return nil
}

// profileNames returns the recognised profile names, sorted, for error
// messages.
func profileNames() []string {
	names := []string{string(ProfileCompact), string(ProfileProduction), string(ProfileDevelopment)}
	sort.Strings(names)
	return names
}
