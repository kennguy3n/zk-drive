package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/tenantctx"
)

// Connect opens a pgx connection pool against the supplied DSN. The
// pool is configured with a PrepareConn hook that binds the
// `app.workspace_id` GUC on every acquire to the workspace UUID
// stored on the caller's context (via tenantctx.WithWorkspaceID).
// Migration 024_row_level_security.up.sql relies on that GUC for
// its tenant_isolation policies; when no workspace is set on the
// context the hook clears the GUC so unauthenticated paths (login,
// signup, public share-link resolution) and background workers fall
// back to the RLS bypass branch.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.PrepareConn = bindTenantGUC

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pgx pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// bindTenantGUC is invoked by pgxpool every time a connection is
// checked out. It overwrites the session-level `app.workspace_id`
// GUC with the workspace UUID on ctx, or clears it (empty string)
// when no workspace is bound. Unconditional overwrite is required
// because the GUC is session-scoped: a previous owner's tenant must
// not leak to the next acquirer.
//
// Returning (true, error) signals pgxpool to release the connection
// back to the pool while propagating the error to the caller — the
// SET runs against a healthy connection, and a SET failure indicates
// a transient backend issue, not a corrupted conn. A subsequent
// acquire will re-run the hook on a fresh checkout.
func bindTenantGUC(ctx context.Context, conn *pgx.Conn) (bool, error) {
	value := ""
	if wsID, ok := tenantctx.WorkspaceIDFromContext(ctx); ok {
		value = wsID.String()
	}
	if _, err := conn.Exec(ctx, "SELECT set_config('app.workspace_id', $1, false)", value); err != nil {
		return true, fmt.Errorf("bind app.workspace_id: %w", err)
	}
	return true, nil
}

// migrateAdvisoryLockKey is the constant passed to pg_advisory_lock so
// concurrent Migrate calls (e.g. two replicas of the migrate Job during a
// blue/green deploy, or a manual `migrate` invocation racing with a Job)
// serialise at the Postgres backend rather than racing on the
// schema_migrations table's primary key. Picked as a fixed, never-reused
// 64-bit value so it can't collide with application-level advisory locks
// (which use the same namespace).
const migrateAdvisoryLockKey int64 = 0x5a4b44524956534D // 'ZKDRIVSM' ASCII

// MinRequiredMigrationVersion is the lowest schema version that the
// server / worker binaries are guaranteed to function against. When a
// pod boots, RequireMinMigrationVersion is called to verify that the
// database has at least this version applied — if not, the pod fails
// fast with a clear error instead of running queries against a stale
// schema (which would silently fall back to RLS-bypass, miss table
// columns, or hit "column does not exist" mid-request).
//
// Bump this value alongside any migration that introduces a column /
// table / RLS policy that the running code REQUIRES (not just
// optionally consumes). The migrate binary is allowed to run against
// any older state — its job is to bring the database up to HEAD; only
// the server/worker binaries gate on this.
const MinRequiredMigrationVersion = "024_row_level_security"

// ErrMigrationsOutOfDate is returned by RequireMinMigrationVersion when
// the database is missing one or more migrations that the binary
// requires. Surfaced as a sentinel so callers (and tests) can
// distinguish "db not yet migrated" from generic startup failures.
var ErrMigrationsOutOfDate = errors.New("database migrations are out of date: run the migrate binary first")

// RequireMinMigrationVersion verifies that MinRequiredMigrationVersion
// has been applied to the database. Returns ErrMigrationsOutOfDate
// if it has not.
//
// This is a spot-check on the named version, NOT a gap-scan of every
// predecessor (1..N-1). Migrations are applied in lexicographic
// order by Migrate() under an advisory lock that serialises
// concurrent runs, so the presence of version N implies versions
// 1..N-1 are also present unless an operator has manually mutated
// schema_migrations. The trade-off is intentional: a gap-scan would
// cost an extra round-trip per startup for a check that catches a
// failure mode (manual row deletion) we don't actually defend
// against at any other layer. WS-18 (down-migration CI) is the
// right place to add gap detection if we ever want it.
//
// This is the entrypoint check both cmd/server and cmd/worker run at
// startup, replacing the old behaviour of calling Migrate() inline.
// Separating "apply migrations" from "check migrations applied" lets
// us run the migrate binary as a Kubernetes Job (or Compose service)
// while the runtime pods refuse to serve traffic against a stale db.
func RequireMinMigrationVersion(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("nil pool")
	}
	var applied bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = 'schema_migrations'
		)
	`).Scan(&applied)
	if err != nil {
		return fmt.Errorf("probe schema_migrations existence: %w", err)
	}
	if !applied {
		return fmt.Errorf("%w: schema_migrations table not found (no migrations have ever been applied)", ErrMigrationsOutOfDate)
	}
	err = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, MinRequiredMigrationVersion).Scan(&applied)
	if err != nil {
		return fmt.Errorf("probe schema_migrations for %s: %w", MinRequiredMigrationVersion, err)
	}
	if !applied {
		return fmt.Errorf("%w: required version %s is not in schema_migrations", ErrMigrationsOutOfDate, MinRequiredMigrationVersion)
	}
	return nil
}

// Migrate runs all forward SQL migrations in dir that have not yet been
// applied. Migrations are expected to follow the golang-migrate naming
// convention (<version>_<name>.up.sql / <version>_<name>.down.sql). We apply
// them in lexicographic order and record applied versions in a
// schema_migrations table.
//
// Concurrency: Migrate acquires a session-scoped Postgres advisory
// lock keyed on migrateAdvisoryLockKey on a dedicated connection
// before doing any work. Two Migrate calls against the same database
// (e.g. two migrate Job pods during a blue/green deploy, or an
// operator running the migrate binary while a Job is already
// running) will serialise at the lock acquire — the second caller
// blocks until the first releases (on normal completion, error, or
// connection death). This is what makes it safe to run Migrate as a
// standalone Job without an external mutex.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock connection: %w", err)
	}

	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateAdvisoryLockKey); err != nil {
		lockConn.Release()
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		// Try the explicit unlock first — happy path returns a
		// clean, lock-free connection to the pool, reusable for any
		// subsequent Migrate() call from the same long-lived process
		// (e.g. the integration test harness re-running setup
		// between subtests, or a future caller in a worker that
		// re-validates the schema).
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := lockConn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrateAdvisoryLockKey); err == nil {
			lockConn.Release()
			return
		}
		// Unlock failed (typically the parent ctx was cancelled
		// mid-migration). Don't return the conn to the pool with a
		// stale advisory lock attached — future Migrate() calls on
		// the same pool could block indefinitely on the leaked
		// lock. Hijack the conn out of the pool and close the raw
		// pgx connection so the session ends and Postgres releases
		// the lock at the backend.
		rawConn := lockConn.Hijack()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = rawConn.Close(closeCtx)
	}()

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version TEXT PRIMARY KEY,
        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	var upFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".up.sql") {
			upFiles = append(upFiles, name)
		}
	}
	sort.Strings(upFiles)

	applied := map[string]struct{}{}
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("query applied migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied migration: %w", err)
		}
		applied[v] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate applied migrations: %w", err)
	}

	for _, name := range upFiles {
		version := strings.TrimSuffix(name, ".up.sql")
		if _, ok := applied[version]; ok {
			continue
		}
		path := filepath.Join(dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}

// ErrNoMigrationsDir is returned when the migrations directory cannot be
// located. Exposed so callers can present a clearer error.
var ErrNoMigrationsDir = errors.New("migrations directory not found")
