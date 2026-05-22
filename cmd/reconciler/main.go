// zk-drive reconciler binary — Phase 5 / WS-14.
//
// Standalone entrypoint that recomputes denormalized counters on
// the workspaces table (currently storage_used_bytes) so they
// match the canonical SUM over the files table. Designed to be run
// as a Kubernetes CronJob (or Compose service) on a regular cadence
// — every workspace is reconciled in O(N) UPDATEs gated by row-level
// locks so a hot upload path never blocks the reconciler beyond the
// duration of one workspace's recompute.
//
// Operational characteristics:
//
//   - Idempotent: re-running against a converged database is a
//     no-op (each workspace's row is locked, sum is recomputed,
//     SELECT shows no drift, no UPDATE issued).
//   - Bounded blast radius: the reconciler does NOT mutate the
//     files table — only the workspaces.storage_used_bytes
//     column. A bug here cannot lose user data.
//   - Best-effort across workspaces: a single sick workspace
//     (e.g. row-level deadlock or schema corruption) is logged as
//     an error and skipped; the rest of the population is still
//     reconciled.
//   - Per-workspace failures do NOT flip the exit code: a single
//     bad row doesn't trip K8s CronJob alerting for the whole
//     run; they are surfaced via log output for ad-hoc triage.
//     WS-17 will swap this for a metrics-based alert.
//   - Exits non-zero in three cases: (a) configuration / pool
//     connect failure (the run could not start), (b) the
//     workspaces enumeration query itself failed, and (c) the
//     run was interrupted by SIGTERM / context cancellation
//     before every workspace had been visited. Case (c) is the
//     expected K8s behaviour when activeDeadlineSeconds fires or
//     a Forbid-concurrency replacement Job lands: the previous
//     Job is correctly flagged Failed so the next scheduled tick
//     can pick up where it left off.
//
// Configuration: reads DATABASE_URL from the environment via
// internal/config.Load. Other env vars Load reads (JWT_SECRET, S3_*,
// etc.) are not used here but Load is reused to keep config parsing
// centralised across every entrypoint, same trade-off the migrate
// binary made.
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
	"github.com/kennguy3n/zk-drive/internal/reconciler"
	"github.com/kennguy3n/zk-drive/internal/version"
)

func main() {
	if err := run(); err != nil {
		log.Printf("reconciler: %v", err)
		os.Exit(1)
	}
}

func run() error {
	log.Printf("zk-drive reconciler version=%s", version.Version)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	start := time.Now()
	rc := reconciler.New(pool)
	summary, err := rc.ReconcileAll(ctx)
	if err != nil {
		return fmt.Errorf("reconcile all: %w", err)
	}

	for _, e := range summary.Errors {
		log.Printf("reconciler: workspace=%s err=%v", e.WorkspaceID, e.Err)
	}
	log.Printf("reconciler: completed workspaces=%d updated=%d drift_bytes=%d errors=%d duration=%s",
		summary.Workspaces, summary.Updated, summary.TotalDriftBytes, len(summary.Errors),
		time.Since(start).Round(time.Millisecond))
	return nil
}
