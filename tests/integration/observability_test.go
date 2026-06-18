package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/heartbeat"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/setup"
)

// freshMigratedPool spins up an isolated database, applies every
// migration, and returns a connected pool. Used by the observability
// tests so each exercises a pristine schema without touching the shared
// long-running fixture other integration tests rely on.
func freshMigratedPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	name := freshTestDatabase(t)
	dsn := dsnForDatabase(t, name)

	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fresh db: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := database.Migrate(ctx, pool, findMigrationsDir(t)); err != nil {
		t.Fatalf("migrate fresh db: %v", err)
	}
	return ctx, pool
}

// TestSetupStatusLifecycle walks a fresh deployment through the
// setup-status state machine the wizard depends on.
func TestSetupStatusLifecycle(t *testing.T) {
	ctx, pool := freshMigratedPool(t)

	// Storage configured (the one required capability), nothing else.
	svc := setup.NewService(pool, setup.Capabilities{StorageConfigured: true})

	// 1. Pristine box: needs setup, no admin, no workspace, but the
	//    storage capability is reported configured from config.
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status (pristine): %v", err)
	}
	if st.SetupCompleted || !st.NeedsSetup {
		t.Fatalf("pristine box should need setup, got %+v", st)
	}
	if st.Steps == nil {
		t.Fatal("pristine status must include step detail")
	}
	if st.Steps.AdminAccount.Configured {
		t.Fatal("no admin should exist yet")
	}
	if !st.Steps.Storage.Configured {
		t.Fatal("storage capability should be reported configured")
	}
	if st.Steps.Workspace.Configured {
		t.Fatal("no workspace should exist yet")
	}

	// 2. Create a workspace + admin user.
	var wsID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (name) VALUES ('Acme') RETURNING id`).Scan(&wsID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (workspace_id, email, name, password_hash, role)
		 VALUES ($1, 'admin@acme.test', 'Admin', 'x', 'admin')`, wsID); err != nil {
		t.Fatalf("insert admin: %v", err)
	}

	st, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status (provisioned): %v", err)
	}
	if st.NeedsSetup {
		t.Fatal("a box with a workspace no longer needs setup")
	}
	if st.Steps == nil || !st.Steps.AdminAccount.Configured || !st.Steps.Workspace.Configured {
		t.Fatalf("admin + workspace should be configured, got %+v", st.Steps)
	}

	// 3. Mark complete — flag flips, detail is withheld, completed_at set.
	if err := svc.MarkCompleted(ctx); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	st, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status (complete): %v", err)
	}
	if !st.SetupCompleted || st.NeedsSetup {
		t.Fatalf("completed box should report done, got %+v", st)
	}
	if st.Steps != nil {
		t.Fatal("completed status must NOT leak step detail to anonymous callers")
	}
	if st.CompletedAt == nil {
		t.Fatal("completed_at should be stamped")
	}
	firstCompletedAt := *st.CompletedAt

	// 4. Re-marking is idempotent and preserves the original timestamp.
	time.Sleep(10 * time.Millisecond)
	if err := svc.MarkCompleted(ctx); err != nil {
		t.Fatalf("MarkCompleted (idempotent): %v", err)
	}
	st, _ = svc.Status(ctx)
	if st.CompletedAt == nil || !st.CompletedAt.Equal(firstCompletedAt) {
		t.Fatalf("completed_at must be preserved across re-marks: was %v now %v",
			firstCompletedAt, st.CompletedAt)
	}

	if ok, err := svc.IsCompleted(ctx); err != nil || !ok {
		t.Fatalf("IsCompleted = (%v,%v), want (true,nil)", ok, err)
	}
}

// TestHeartbeatStoreRoundTrip exercises the worker-liveness store the
// dashboard reads: upsert refreshes in place, the
// worst status across instances wins, and the freshest instance's
// detail is surfaced.
func TestHeartbeatStoreRoundTrip(t *testing.T) {
	ctx, pool := freshMigratedPool(t)
	store := heartbeat.NewStore(pool)

	// Two instances of the same worker type, one degraded.
	if err := store.Upsert(ctx, "host-a/1", heartbeat.Beat{
		WorkerType: "scan", Status: heartbeat.StatusOK,
		Detail: map[string]any{"virus_scanning": true},
	}); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if err := store.Upsert(ctx, "host-b/1", heartbeat.Beat{
		WorkerType: "scan", Status: heartbeat.StatusDegraded,
		Detail: map[string]any{"virus_scanning": false},
	}); err != nil {
		t.Fatalf("upsert b: %v", err)
	}
	// A different worker type.
	if err := store.Upsert(ctx, "host-a/1", heartbeat.Beat{
		WorkerType: "preview", Status: heartbeat.StatusOK,
	}); err != nil {
		t.Fatalf("upsert preview: %v", err)
	}

	health, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(health) != 2 {
		t.Fatalf("expected 2 worker types, got %d (%+v)", len(health), health)
	}
	// Sorted by type: preview, scan.
	if health[0].WorkerType != "preview" || health[1].WorkerType != "scan" {
		t.Fatalf("worker types not sorted: %+v", health)
	}
	scan := health[1]
	if scan.Instances != 2 {
		t.Fatalf("scan should aggregate 2 instances, got %d", scan.Instances)
	}
	if scan.Status != heartbeat.StatusDegraded {
		t.Fatalf("scan status should be worst-of (degraded), got %q", scan.Status)
	}

	// Upsert again for host-a to confirm it refreshes rather than
	// inserting a duplicate.
	if err := store.Upsert(ctx, "host-a/1", heartbeat.Beat{
		WorkerType: "scan", Status: heartbeat.StatusOK,
	}); err != nil {
		t.Fatalf("re-upsert a: %v", err)
	}
	health, _ = store.List(ctx)
	for _, h := range health {
		if h.WorkerType == "scan" && h.Instances != 2 {
			t.Fatalf("re-upsert must not create a duplicate instance, got %d", h.Instances)
		}
	}
}

// TestHeartbeatPruneReapsOnlyLongDeadRows verifies Prune deletes rows
// older than PruneRetention (left behind by restarted instances whose
// pid-based id never gets overwritten) while leaving fresh rows — and
// the freshly written instances of a still-running worker type —
// untouched, so the table stays bounded across rolling deploys.
func TestHeartbeatPruneReapsOnlyLongDeadRows(t *testing.T) {
	ctx, pool := freshMigratedPool(t)
	store := heartbeat.NewStore(pool)

	// A fresh, live instance.
	if err := store.Upsert(ctx, "host-live/1", heartbeat.Beat{
		WorkerType: "scan", Status: heartbeat.StatusOK,
	}); err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	// A stale instance from a since-restarted process. Age its row
	// well beyond PruneRetention so Prune must reap exactly it.
	if err := store.Upsert(ctx, "host-dead/9999", heartbeat.Beat{
		WorkerType: "scan", Status: heartbeat.StatusOK,
	}); err != nil {
		t.Fatalf("upsert dead: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_heartbeats
		SET last_seen_at = now() - make_interval(secs => $1)
		WHERE instance_id = 'host-dead/9999'
	`, (heartbeat.PruneRetention + time.Hour).Seconds()); err != nil {
		t.Fatalf("age dead row: %v", err)
	}

	n, err := store.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune reaped %d rows, want exactly 1 (the long-dead instance)", n)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worker_heartbeats`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("expected 1 surviving row (the live instance), got %d", remaining)
	}
}

// TestPreviewSetStatus pins the preview lifecycle column the
// auto-healing worker writes when a job exhausts its retries.
func TestPreviewSetStatus(t *testing.T) {
	ctx, pool := freshMigratedPool(t)
	repo := preview.NewPostgresRepository(pool)

	// Seed the parent rows: workspace -> user -> folder -> file -> version.
	var wsID, userID, folderID, fileID, versionID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (name) VALUES ('Acme') RETURNING id`).Scan(&wsID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (workspace_id, email, name, password_hash, role)
		 VALUES ($1, 'u@acme.test', 'U', 'x', 'member') RETURNING id`, wsID).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO folders (workspace_id, name, created_by)
		 VALUES ($1, 'root', $2) RETURNING id`, wsID, userID).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO files (workspace_id, folder_id, name, mime_type, created_by)
		 VALUES ($1, $2, 'doc.png', 'image/png', $3) RETURNING id`, wsID, folderID, userID).Scan(&fileID); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO file_versions (file_id, object_key, size_bytes, checksum, created_by)
		 VALUES ($1, 'obj/key', 10, 'deadbeef', $2) RETURNING id`, fileID, userID).Scan(&versionID); err != nil {
		t.Fatalf("insert version: %v", err)
	}

	// Mark it failed (the terminal state after PreviewMaxAttempts).
	if err := repo.SetStatus(ctx, versionID, preview.StatusFailed, "decode error: bad header"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	var status, detail string
	if err := pool.QueryRow(ctx,
		`SELECT preview_status, COALESCE(preview_detail,'') FROM file_versions WHERE id = $1`,
		versionID).Scan(&status, &detail); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != preview.StatusFailed {
		t.Fatalf("preview_status = %q, want %q", status, preview.StatusFailed)
	}
	if detail == "" {
		t.Fatal("preview_detail should carry the failure reason")
	}

	// A missing version is a no-op, not an error (row may have been
	// deleted between enqueue and the terminal marking).
	if err := repo.SetStatus(ctx, uuid.New(), preview.StatusFailed, "x"); err != nil {
		t.Fatalf("SetStatus on missing version should be a no-op, got %v", err)
	}
}
