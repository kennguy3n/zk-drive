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

// Migrate runs all forward SQL migrations in dir that have not yet been
// applied. Migrations are expected to follow the golang-migrate naming
// convention (<version>_<name>.up.sql / <version>_<name>.down.sql). We apply
// them in lexicographic order and record applied versions in a
// schema_migrations table.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
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
