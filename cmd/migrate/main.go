// zk-drive migrate binary — Phase 5 / WS-11.
//
// Standalone entrypoint that connects to Postgres and applies every
// pending migration in MigrationsDir (default ./migrations). Designed
// to be run as a Kubernetes Job (or Compose service) ahead of the
// server / worker pods, replacing the previous behaviour of calling
// database.Migrate() inline on every server startup.
//
// Operational characteristics:
//
//   - Idempotent: re-running the binary against an up-to-date
//     database is a no-op (acquires the advisory lock, finds nothing
//     to apply, releases, exits 0).
//   - Safe under concurrency: database.Migrate uses a session-scoped
//     Postgres advisory lock keyed on a fixed constant, so two
//     concurrent migrate Job pods (or an operator racing with a Job)
//     serialise at the database — the second caller blocks until the
//     first commits or errors out.
//   - Exits non-zero on any failure with a clear message so a Job
//     orchestrator can surface "deploy failed" rather than letting
//     the server pods stall on RequireMinMigrationVersion.
//
// Configuration: reads DATABASE_URL and MIGRATIONS_DIR from the
// environment via internal/config.Load. The other env vars Load reads
// (JWT_SECRET, S3_*, etc.) are not used by this binary but Load is
// reused to keep config parsing centralised across every entrypoint.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kennguy3n/zk-drive/internal/config"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/version"
)

func main() {
	if err := run(); err != nil {
		log.Printf("migrate: %v", err)
		os.Exit(1)
	}
}

func run() error {
	log.Printf("zk-drive migrate version=%s", version.Version)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// SIGTERM-aware: a K8s Job hitting activeDeadlineSeconds (or an
	// operator hitting Ctrl-C) cancels the context so the in-flight
	// migration can roll back cleanly. The advisory lock is
	// session-scoped and auto-releases when the connection closes;
	// database.Migrate also issues an explicit unlock as
	// defence-in-depth.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	start := time.Now()
	if err := database.Migrate(ctx, pool, cfg.MigrationsDir); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	log.Printf("migrate: completed in %s (migrations_dir=%s)", time.Since(start).Round(time.Millisecond), cfg.MigrationsDir)
	return nil
}
