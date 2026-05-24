package integration

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-drive/internal/database"
)

// TestRequireMinMigrationVersionRejectsFreshDatabase verifies that a
// Postgres instance with no schema_migrations table at all (i.e. an
// operator forgot to run the migrate Job before the server) fails
// fast with ErrMigrationsOutOfDate, rather than starting up and
// returning errors mid-request.
//
// The test sandbox is a per-test Postgres database created on the
// fly so we can reach a "no migrations applied" state without
// affecting the long-running shared fixture other integration
// tests rely on.
func TestRequireMinMigrationVersionRejectsFreshDatabase(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	dbName := freshTestDatabase(t)
	dsn := dsnForDatabase(t, dbName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fresh db: %v", err)
	}
	t.Cleanup(pool.Close)

	err = database.RequireMinMigrationVersion(ctx, pool)
	if err == nil {
		t.Fatalf("RequireMinMigrationVersion(empty db) returned nil, want ErrMigrationsOutOfDate")
	}
	if !errors.Is(err, database.ErrMigrationsOutOfDate) {
		t.Fatalf("RequireMinMigrationVersion(empty db) returned %v, want wrapping ErrMigrationsOutOfDate", err)
	}
}

// TestRequireMinMigrationVersionAcceptsMigratedDatabase verifies the
// opposite — once Migrate has applied every up file in
// migrations/, the precondition passes. This pins the contract
// that MinRequiredMigrationVersion is a known-applied version on a
// fully-migrated database.
func TestRequireMinMigrationVersionAcceptsMigratedDatabase(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	dbName := freshTestDatabase(t)
	dsn := dsnForDatabase(t, dbName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fresh db: %v", err)
	}
	t.Cleanup(pool.Close)

	migrationsDir := findMigrationsDir(t)
	if err := database.Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := database.RequireMinMigrationVersion(ctx, pool); err != nil {
		t.Fatalf("RequireMinMigrationVersion(migrated db): %v", err)
	}
	// audit-archiver has its own precondition constant pointing at
	// migration 027. Pin that on a fully-migrated database too so
	// future migration renumbering doesn't silently leave the
	// archiver gated on a stale version.
	if err := database.RequireMinMigrationVersionFor(
		ctx, pool, database.MinRequiredMigrationVersionAuditArchiver,
	); err != nil {
		t.Fatalf("RequireMinMigrationVersionFor(audit-archiver, migrated db): %v", err)
	}
}

// TestRequireMinMigrationVersionForCustomVersion verifies the
// parameterised form rejects a synthetic future version that does
// NOT exist in schema_migrations, even on a fully-migrated database.
// Pins the contract that any new binary adding a higher-watermark
// constant will fail-fast against an older schema rather than
// silently passing on the server/worker baseline.
func TestRequireMinMigrationVersionForCustomVersion(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	dbName := freshTestDatabase(t)
	dsn := dsnForDatabase(t, dbName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fresh db: %v", err)
	}
	t.Cleanup(pool.Close)

	migrationsDir := findMigrationsDir(t)
	if err := database.Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	err = database.RequireMinMigrationVersionFor(ctx, pool, "999_future_migration_that_does_not_exist")
	if err == nil {
		t.Fatalf("RequireMinMigrationVersionFor(unknown version) returned nil, want ErrMigrationsOutOfDate")
	}
	if !errors.Is(err, database.ErrMigrationsOutOfDate) {
		t.Fatalf("RequireMinMigrationVersionFor(unknown version) returned %v, want wrapping ErrMigrationsOutOfDate", err)
	}
}

// TestMigrateAdvisoryLockSerializesConcurrentRuns starts two Migrate
// goroutines against the same fresh database simultaneously and
// verifies (a) both return nil and (b) the second one observes a
// fully-migrated state on entry (i.e. it ran after the first, not
// concurrently with it). Without the advisory lock the two would
// race on schema_migrations PK conflicts; with it they serialise at
// the lock acquire and the second is a no-op.
func TestMigrateAdvisoryLockSerializesConcurrentRuns(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	dbName := freshTestDatabase(t)
	dsn := dsnForDatabase(t, dbName)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Two independent pools so each goroutine acquires its own
	// advisory-lock connection (a single pool would route both
	// Acquire() calls through the same MaxConns=10 budget but
	// could potentially hand them the same backend; separate
	// pools mirrors what two migrate Job pods in K8s would
	// actually do).
	poolA, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pool A: %v", err)
	}
	t.Cleanup(poolA.Close)
	poolB, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pool B: %v", err)
	}
	t.Cleanup(poolB.Close)

	migrationsDir := findMigrationsDir(t)

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]error, 2)
	start := make(chan struct{})

	go func() {
		defer wg.Done()
		<-start
		errs[0] = database.Migrate(ctx, poolA, migrationsDir)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = database.Migrate(ctx, poolB, migrationsDir)
	}()

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate goroutine %d failed: %v", i, err)
		}
	}

	// Verify the schema is fully migrated regardless of which
	// goroutine won the lock race.
	if err := database.RequireMinMigrationVersion(ctx, poolA); err != nil {
		t.Fatalf("post-concurrent RequireMinMigrationVersion: %v", err)
	}

	// Sanity: schema_migrations row count should equal the number
	// of .up.sql files in migrationsDir (each migration applied
	// exactly once). If the advisory lock had failed both
	// goroutines would have tried INSERT-ing and the second
	// would have errored on the PK, which we already asserted
	// against. This is the positive shape check.
	var count int
	if err := poolA.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	upCount := countUpFiles(t, migrationsDir)
	if count != upCount {
		t.Fatalf("schema_migrations row count = %d, want %d (one per .up.sql file)", count, upCount)
	}
}
