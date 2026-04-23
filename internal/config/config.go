package config

import (
	"errors"
	"os"
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
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		JWTSecret:     os.Getenv("JWT_SECRET"),
		ListenAddr:    getEnvDefault("LISTEN_ADDR", ":8080"),
		S3Endpoint:    os.Getenv("S3_ENDPOINT"),
		S3Bucket:      os.Getenv("S3_BUCKET"),
		S3AccessKey:   os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:   os.Getenv("S3_SECRET_KEY"),
		MigrationsDir: getEnvDefault("MIGRATIONS_DIR", "migrations"),
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
