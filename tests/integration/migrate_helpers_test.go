package integration

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// freshTestDatabase creates a new Postgres database with a unique
// name derived from the calling test's name, drops it on cleanup, and
// returns the database name. This gives migrate integration tests a
// blank slate without affecting the shared long-running fixture other
// tests rely on.
//
// The function connects to the admin database using
// TEST_DATABASE_URL, creates the new database, then returns. The
// caller is expected to pass the returned name to dsnForDatabase()
// to obtain a pool-compatible DSN that points at the fresh database.
func freshTestDatabase(t *testing.T) string {
	t.Helper()

	baseDSN := os.Getenv("TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Fatal("TEST_DATABASE_URL is required for freshTestDatabase")
	}

	// Derive a unique database name from the test name. Replace
	// characters Postgres doesn't allow in identifiers with
	// underscores, then truncate to Postgres' 63-byte NAMEDATALEN.
	name := "migrate_test_" + strings.ToLower(
		strings.NewReplacer("/", "_", "-", "_", " ", "_").Replace(t.Name()),
	)
	if len(name) > 63 {
		name = name[:63]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Admin connection to the base database (the one TEST_DATABASE_URL
	// points at). We use a raw pgxpool rather than database.Connect
	// because Connect registers PrepareConn hooks, and the CREATE
	// DATABASE statement doesn't need (or want) tenant-GUC wiring.
	adminPool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer adminPool.Close()

	// DROP IF EXISTS so re-runs of the same test name are
	// idempotent (e.g. when debugging a flaky test locally).
	if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgQuoteIdent(name)); err != nil {
		t.Fatalf("drop stale test db %q: %v", name, err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgQuoteIdent(name)); err != nil {
		t.Fatalf("create test db %q: %v", name, err)
	}
	t.Cleanup(func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()
		pool2, err := pgxpool.New(ctx2, baseDSN)
		if err != nil {
			t.Logf("cleanup: admin reconnect failed: %v", err)
			return
		}
		defer pool2.Close()
		// Terminate any lingering connections so DROP succeeds.
		// pg_stat_activity.datname is a regular column so it accepts
		// a $1 placeholder (unlike DROP DATABASE which needs the
		// identifier interpolated and is handled via pgQuoteIdent
		// below).
		_, _ = pool2.Exec(ctx2,
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
			name)
		if _, err := pool2.Exec(ctx2, "DROP DATABASE IF EXISTS "+pgQuoteIdent(name)); err != nil {
			t.Logf("cleanup: drop test db %q: %v", name, err)
		}
	})
	return name
}

// dsnForDatabase rewrites TEST_DATABASE_URL to point at the given
// database name, preserving host, port, user, password, and query
// parameters from the base DSN.
func dsnForDatabase(t *testing.T, dbName string) string {
	t.Helper()
	baseDSN := os.Getenv("TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Fatal("TEST_DATABASE_URL is required for dsnForDatabase")
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + dbName
	return u.String()
}

// pgQuoteIdent double-quotes a Postgres identifier to avoid SQL
// injection and handle names with special characters. We do NOT use
// this for values — only object names in DDL.
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// countUpFiles returns the number of .up.sql files in dir.
func countUpFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			n++
		}
	}
	return n
}
