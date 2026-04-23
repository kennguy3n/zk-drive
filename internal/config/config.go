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
	MigrationsDir string
}

// Load reads configuration from environment variables and returns a populated
// Config. It returns an error if any required variable is missing or empty.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		JWTSecret:     os.Getenv("JWT_SECRET"),
		ListenAddr:    getEnvDefault("LISTEN_ADDR", ":8080"),
		S3Endpoint:    os.Getenv("S3_ENDPOINT"),
		S3Bucket:      os.Getenv("S3_BUCKET"),
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
	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
