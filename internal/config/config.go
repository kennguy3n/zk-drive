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
