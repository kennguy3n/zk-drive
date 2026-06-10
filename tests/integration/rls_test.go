package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/zk-drive/internal/tenantctx"
)

// The RLS integration test owns a non-superuser role created on
// demand below. Postgres bypasses row-level security for
// superusers, and the CI test fixture (POSTGRES_USER=zkdrive) is a
// superuser of the database, so verifying the tenant_isolation
// policies actually fire requires a separate, lower-privilege role.
// We grant the same SELECT/INSERT/UPDATE/DELETE the app needs at
// runtime; nothing in the rest of the test suite touches this role.

// rlsTenantTables is the canonical list of tables RLS protects, kept
// here (rather than imported from somewhere) so any future migration
// that adds a tenant table is forced through this test by failing
// loudly. The list mirrors migrations/024_row_level_security.up.sql
// (direct + workspaces + version/preview chains) plus change_log,
// which migration 033_partition_large_tables.up.sql brings under the
// tenant_isolation policy when it hash-partitions the table, plus the
// push-subscription tables webpush_subscriptions (migration 038) and
// device_push_tokens (migration 039), which carry the same policy.
var rlsTenantTables = []string{
	"workspaces",
	"users",
	"folders",
	"files",
	"file_versions",
	"file_previews",
	"permissions",
	"activity_log",
	"share_links",
	"guest_invites",
	"client_rooms",
	"notifications",
	"audit_log",
	"change_log",
	"retention_policies",
	"file_tags",
	"workspace_plans",
	"usage_events",
	"workspace_storage_credentials",
	"kchat_room_folders",
	"webpush_subscriptions",
	"device_push_tokens",
}

// ensureRLSTestRole creates rls_test_role idempotently and grants
// the table privileges needed to exercise the tenant_isolation
// policies. The role is NOLOGIN — it is only ever assumed via SET
// LOCAL ROLE inside a transaction owned by the (superuser) test
// connection.
func ensureRLSTestRole(t *testing.T, env *testEnv) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := env.pool.Exec(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rls_test_role') THEN
				CREATE ROLE rls_test_role NOLOGIN;
			END IF;
		END$$;`); err != nil {
		t.Fatalf("ensure rls_test_role: %v", err)
	}

	// GRANT EXECUTE on the helper function is required because
	// every policy USING/WITH CHECK clause calls
	// app_current_workspace_id(). Without this grant the role
	// can't even evaluate the policy and queries fail with
	// "permission denied for function".
	if _, err := env.pool.Exec(ctx,
		`GRANT EXECUTE ON FUNCTION app_current_workspace_id() TO rls_test_role`); err != nil {
		t.Fatalf("grant execute on app_current_workspace_id: %v", err)
	}

	// Build a single GRANT statement so the role list stays
	// authoritative in rlsTenantTables.
	stmt := "GRANT SELECT, INSERT, UPDATE, DELETE ON " +
		strings.Join(rlsTenantTables, ", ") +
		" TO rls_test_role"
	if _, err := env.pool.Exec(ctx, stmt); err != nil {
		t.Fatalf("grant tenant tables to rls_test_role: %v", err)
	}
}

// runAsRLSRole runs fn inside a fresh transaction with the role
// switched to rls_test_role, so RLS policies fire. The transaction
// is always rolled back — fn must not rely on its writes being
// visible after return.
//
// `wsID` is bound on the connection via the same tenantctx hook the
// production code uses (pgxpool PrepareConn → SET app.workspace_id),
// so the test exercises the real production wiring rather than a
// hand-rolled SET inside the transaction.
//
// fn callers can pass context.Background() (rather than a workspace-
// bound context) to tx.QueryRow / tx.Exec inside fn: the GUC was set
// at session level by the PrepareConn hook when Begin acquired this
// connection, and a session-level GUC persists across every operation
// on that connection regardless of what context is threaded through
// pgx. The context passed to tx.QueryRow only governs cancellation /
// timeout, NOT the tenant scope.
func runAsRLSRole(t *testing.T, env *testEnv, wsID uuid.UUID, fn func(t *testing.T, tx pgx.Tx)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if wsID != uuid.Nil {
		ctx = tenantctx.WithWorkspaceID(ctx, wsID)
	}
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE rls_test_role"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	fn(t, tx)
}

// TestRLSEnforcesCrossTenantIsolation walks the full defence-in-depth
// promise of migration 024_row_level_security.up.sql:
//
//  1. Sign up two workspaces with one folder each (using the real
//     signup + create-folder API paths so RLS runs against the
//     actual production wiring, including the tenantctx →
//     PrepareConn → app.workspace_id chain).
//  2. With the rls_test_role role active and workspace A bound,
//     verify that an UNFILTERED query against the tenant tables
//     returns ONLY workspace A's rows — proving that even a future
//     bug that omits the WHERE clause cannot leak cross-tenant
//     data.
//  3. With no workspace bound, verify the bypass branch lets
//     migrations / workers / pre-auth handlers see all rows.
//  4. Verify that an INSERT carrying a foreign workspace_id is
//     rejected by the WITH CHECK clause, not silently accepted.
func TestRLSEnforcesCrossTenantIsolation(t *testing.T) {
	env := setupEnv(t)
	ensureRLSTestRole(t, env)

	tokA := env.signupAndLogin("WorkspaceA", "alice@rls.test", "Alice", "password-aa")
	tokB := env.signupAndLogin("WorkspaceB", "bob@rls.test", "Bob", "password-bb")

	wsA := uuid.MustParse(tokA.WorkspaceID)
	wsB := uuid.MustParse(tokB.WorkspaceID)

	folderA := createFolder(t, env, tokA.Token, nil, "FolderA")
	folderB := createFolder(t, env, tokB.Token, nil, "FolderB")

	// 1. With workspace A bound, the unfiltered folder query
	//    must return exactly folder A.
	runAsRLSRole(t, env, wsA, func(t *testing.T, tx pgx.Tx) {
		var count int
		if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM folders").Scan(&count); err != nil {
			t.Fatalf("count folders (wsA bound): %v", err)
		}
		if count != 1 {
			t.Fatalf("wsA bound: expected 1 folder visible, got %d", count)
		}

		var visibleID uuid.UUID
		if err := tx.QueryRow(context.Background(), "SELECT id FROM folders").Scan(&visibleID); err != nil {
			t.Fatalf("select folders (wsA bound): %v", err)
		}
		if visibleID != folderA.ID {
			t.Fatalf("wsA bound: expected folder %s, got %s", folderA.ID, visibleID)
		}

		// Workspaces table — same isolation, keyed on id rather
		// than workspace_id.
		var wsCount int
		if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM workspaces").Scan(&wsCount); err != nil {
			t.Fatalf("count workspaces (wsA bound): %v", err)
		}
		if wsCount != 1 {
			t.Fatalf("wsA bound: expected 1 workspace visible, got %d", wsCount)
		}
	})

	// 2. Symmetric check for workspace B.
	runAsRLSRole(t, env, wsB, func(t *testing.T, tx pgx.Tx) {
		var visibleID uuid.UUID
		if err := tx.QueryRow(context.Background(), "SELECT id FROM folders").Scan(&visibleID); err != nil {
			t.Fatalf("select folders (wsB bound): %v", err)
		}
		if visibleID != folderB.ID {
			t.Fatalf("wsB bound: expected folder %s, got %s", folderB.ID, visibleID)
		}
	})

	// 3. With no workspace bound — the bypass path — the role
	//    must see both folders. This is the path migrations,
	//    workers, and pre-auth handlers rely on.
	runAsRLSRole(t, env, uuid.Nil, func(t *testing.T, tx pgx.Tx) {
		var count int
		if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM folders").Scan(&count); err != nil {
			t.Fatalf("count folders (bypass): %v", err)
		}
		if count != 2 {
			t.Fatalf("bypass: expected both folders visible, got %d", count)
		}
	})

	// 4. WITH CHECK enforcement — an INSERT that lies about its
	//    workspace_id is rejected, even when the role thinks it
	//    has full privileges on the table. We can't easily INSERT
	//    a folder without violating other constraints, so probe
	//    the policy via activity_log which has minimal mandatory
	//    columns.
	runAsRLSRole(t, env, wsA, func(t *testing.T, tx pgx.Tx) {
		ownerID := uuid.MustParse(tokA.UserID)
		// Sanity: an INSERT scoped to the active workspace
		// (wsA) succeeds.
		if _, err := tx.Exec(context.Background(),
			`INSERT INTO activity_log (workspace_id, user_id, action, resource_type, resource_id)
			 VALUES ($1, $2, 'rls_test', 'folder', $3)`,
			wsA, ownerID, folderA.ID); err != nil {
			t.Fatalf("insert into own workspace (should succeed): %v", err)
		}

		// The same INSERT with the foreign workspace_id must be
		// rejected by the policy's WITH CHECK clause.
		_, err := tx.Exec(context.Background(),
			`INSERT INTO activity_log (workspace_id, user_id, action, resource_type, resource_id)
			 VALUES ($1, $2, 'rls_test_evil', 'folder', $3)`,
			wsB, ownerID, folderB.ID)
		if err == nil {
			t.Fatalf("expected RLS to reject cross-tenant INSERT, got nil")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") &&
			!strings.Contains(strings.ToLower(err.Error()), "row level security") {
			t.Fatalf("expected row-level security error, got %v", err)
		}
	})

	// 5. Migration 033 hash-partitions activity_log, audit_log, and
	//    change_log by workspace_id (64 partitions each). Partitioning
	//    must stay transparent to RLS: the tenant_isolation policy on
	//    the partitioned parent has to keep isolating rows across
	//    tenants even though inserts now route to per-hash partitions
	//    and selects prune to them.
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pcancel()

	assertPartitioned := func(table string, wantParts int) {
		t.Helper()
		var relkind string
		if err := env.pool.QueryRow(pctx,
			`SELECT relkind::text FROM pg_class
			 WHERE relname = $1 AND relnamespace = 'public'::regnamespace`,
			table).Scan(&relkind); err != nil {
			t.Fatalf("relkind for %s: %v", table, err)
		}
		if relkind != "p" {
			t.Fatalf("%s: relkind = %q, want \"p\" (a partitioned table)", table, relkind)
		}
		var parts int
		if err := env.pool.QueryRow(pctx,
			`SELECT count(*) FROM pg_inherits WHERE inhparent = $1::regclass`,
			table).Scan(&parts); err != nil {
			t.Fatalf("partition count for %s: %v", table, err)
		}
		if parts != wantParts {
			t.Fatalf("%s: %d partitions, want %d", table, parts, wantParts)
		}
	}
	assertPartitioned("activity_log", 64)
	assertPartitioned("audit_log", 64)
	assertPartitioned("change_log", 64)

	// Seed one committed row per workspace into each of the partitioned
	// tables via the superuser pool (which bypasses RLS), then verify a
	// tenant-bound, unfiltered read through rls_test_role sees only its
	// own workspace's row — proving partition routing did not defeat
	// the tenant_isolation policy. change_log is included because
	// migration 033 adds to it the tenant_isolation policy it never
	// had, so this exercises that newly-added policy on a partitioned
	// table.
	ownerA := uuid.MustParse(tokA.UserID)
	ownerB := uuid.MustParse(tokB.UserID)
	actA, actB := uuid.New(), uuid.New()
	audA, audB := uuid.New(), uuid.New()
	chgA, chgB := uuid.New(), uuid.New()

	seeds := []struct {
		query string
		args  []interface{}
	}{
		{`INSERT INTO activity_log (id, workspace_id, user_id, action, resource_type, resource_id)
		  VALUES ($1, $2, $3, 'part_seed', 'folder', $4)`, []interface{}{actA, wsA, ownerA, folderA.ID}},
		{`INSERT INTO activity_log (id, workspace_id, user_id, action, resource_type, resource_id)
		  VALUES ($1, $2, $3, 'part_seed', 'folder', $4)`, []interface{}{actB, wsB, ownerB, folderB.ID}},
		{`INSERT INTO audit_log (id, workspace_id, actor_id, action)
		  VALUES ($1, $2, $3, 'part_seed')`, []interface{}{audA, wsA, ownerA}},
		{`INSERT INTO audit_log (id, workspace_id, actor_id, action)
		  VALUES ($1, $2, $3, 'part_seed')`, []interface{}{audB, wsB, ownerB}},
		{`INSERT INTO change_log (workspace_id, actor_id, kind, op, resource_id, name)
		  VALUES ($1, $2, 'folder', 'create', $3, 'part_seed')`, []interface{}{wsA, ownerA, chgA}},
		{`INSERT INTO change_log (workspace_id, actor_id, kind, op, resource_id, name)
		  VALUES ($1, $2, 'folder', 'create', $3, 'part_seed')`, []interface{}{wsB, ownerB, chgB}},
	}
	for _, s := range seeds {
		if _, err := env.pool.Exec(pctx, s.query, s.args...); err != nil {
			t.Fatalf("seed partitioned row: %v", err)
		}
	}

	// These rows are committed outside any rolled-back transaction (a
	// tenant-bound connection has to observe them), so delete them when
	// the test ends to keep the run idempotent and avoid polluting other
	// tests. A fresh context is used because pctx is cancelled by its
	// deferred cancel before Cleanup funcs run.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		// Constrain each delete by workspace_id as well as the row id.
		// The deletes run on the superuser pool, which bypasses RLS, so
		// without an explicit workspace_id predicate the planner has no
		// hash-partition-key equality and must scan all 64 partitions.
		// Adding workspace_id = ANY(...) lets it prune to just the two
		// partitions holding wsA/wsB. (The read assertions below already
		// prune via the RLS policy's workspace_id = app_current_workspace_id().)
		wss := []uuid.UUID{wsA, wsB}
		cleanups := []struct {
			query string
			args  []interface{}
		}{
			{`DELETE FROM activity_log WHERE workspace_id = ANY($1) AND id = ANY($2)`, []interface{}{wss, []uuid.UUID{actA, actB}}},
			{`DELETE FROM audit_log WHERE workspace_id = ANY($1) AND id = ANY($2)`, []interface{}{wss, []uuid.UUID{audA, audB}}},
			{`DELETE FROM change_log WHERE workspace_id = ANY($1) AND resource_id = ANY($2)`, []interface{}{wss, []uuid.UUID{chgA, chgB}}},
		}
		for _, c := range cleanups {
			if _, err := env.pool.Exec(cctx, c.query, c.args...); err != nil {
				t.Logf("cleanup partitioned seed rows: %v", err)
			}
		}
	})

	// With workspace A bound, A's seeded rows are visible and B's are
	// hidden — even when B's row id is named explicitly in the WHERE.
	runAsRLSRole(t, env, wsA, func(t *testing.T, tx pgx.Tx) {
		cases := []struct {
			table     string
			idCol     string
			ownID     uuid.UUID
			foreignID uuid.UUID
		}{
			{"activity_log", "id", actA, actB},
			{"audit_log", "id", audA, audB},
			{"change_log", "resource_id", chgA, chgB},
		}
		for _, c := range cases {
			var own int
			if err := tx.QueryRow(context.Background(),
				"SELECT count(*) FROM "+c.table+" WHERE "+c.idCol+" = $1", c.ownID).Scan(&own); err != nil {
				t.Fatalf("count own %s row (wsA bound): %v", c.table, err)
			}
			if own != 1 {
				t.Fatalf("wsA bound: expected own %s row visible, got %d", c.table, own)
			}

			var foreign int
			if err := tx.QueryRow(context.Background(),
				"SELECT count(*) FROM "+c.table+" WHERE "+c.idCol+" = $1", c.foreignID).Scan(&foreign); err != nil {
				t.Fatalf("count foreign %s row (wsA bound): %v", c.table, err)
			}
			if foreign != 0 {
				t.Fatalf("wsA bound: RLS leaked a cross-tenant %s row on the partitioned table (got %d)", c.table, foreign)
			}
		}
	})
}

// TestRLSAppliesToFileVersionsViaParentFiles verifies the EXISTS
// subquery on the file_versions policy — which carries no
// workspace_id of its own — still scopes correctly via files.
func TestRLSAppliesToFileVersionsViaParentFiles(t *testing.T) {
	env := setupEnv(t)
	ensureRLSTestRole(t, env)

	tokA := env.signupAndLogin("VersionsA", "va@rls.test", "Ann", "password-vva")
	tokB := env.signupAndLogin("VersionsB", "vb@rls.test", "Ben", "password-vvb")
	wsA := uuid.MustParse(tokA.WorkspaceID)
	wsB := uuid.MustParse(tokB.WorkspaceID)
	userA := uuid.MustParse(tokA.UserID)
	userB := uuid.MustParse(tokB.UserID)

	// Create one folder, one file, and one file_version in each
	// workspace directly via the pool. This bypasses the API
	// (which would generate a real version row through
	// ConfirmUpload) — sufficient for the isolation test because
	// RLS doesn't care which code path inserted the row.
	folderA := createFolder(t, env, tokA.Token, nil, "FA")
	folderB := createFolder(t, env, tokB.Token, nil, "FB")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fileA := uuid.New()
	fileB := uuid.New()
	verA := uuid.New()
	verB := uuid.New()
	insertFile := `INSERT INTO files (id, workspace_id, folder_id, name, created_by) VALUES ($1, $2, $3, $4, $5)`
	if _, err := env.pool.Exec(ctx, insertFile, fileA, wsA, folderA.ID, "a.txt", userA); err != nil {
		t.Fatalf("insert file A: %v", err)
	}
	if _, err := env.pool.Exec(ctx, insertFile, fileB, wsB, folderB.ID, "b.txt", userB); err != nil {
		t.Fatalf("insert file B: %v", err)
	}
	insertVersion := `INSERT INTO file_versions (id, file_id, version_number, object_key, size_bytes, checksum, created_by) VALUES ($1, $2, 1, $3, 0, 'sha256:x', $4)`
	if _, err := env.pool.Exec(ctx, insertVersion, verA, fileA, "kA/a.bin", userA); err != nil {
		t.Fatalf("insert version A: %v", err)
	}
	if _, err := env.pool.Exec(ctx, insertVersion, verB, fileB, "kB/b.bin", userB); err != nil {
		t.Fatalf("insert version B: %v", err)
	}

	runAsRLSRole(t, env, wsA, func(t *testing.T, tx pgx.Tx) {
		var visibleID uuid.UUID
		if err := tx.QueryRow(context.Background(), "SELECT id FROM file_versions").Scan(&visibleID); err != nil {
			t.Fatalf("select file_versions (wsA): %v", err)
		}
		if visibleID != verA {
			t.Fatalf("wsA bound: expected version %s, got %s", verA, visibleID)
		}
	})
	runAsRLSRole(t, env, wsB, func(t *testing.T, tx pgx.Tx) {
		var visibleID uuid.UUID
		if err := tx.QueryRow(context.Background(), "SELECT id FROM file_versions").Scan(&visibleID); err != nil {
			t.Fatalf("select file_versions (wsB): %v", err)
		}
		if visibleID != verB {
			t.Fatalf("wsB bound: expected version %s, got %s", verB, visibleID)
		}
	})
}

// TestRLSAppCurrentWorkspaceIDReflectsContext is a direct sanity
// check on the tenantctx → PrepareConn → app.workspace_id chain.
// It guards against a future refactor that breaks the wiring such
// that the GUC silently stays at "" for authenticated requests —
// at which point RLS would silently fall through to the bypass
// branch and the protections in this PR would disappear.
func TestRLSAppCurrentWorkspaceIDReflectsContext(t *testing.T) {
	env := setupEnv(t)
	ensureRLSTestRole(t, env)

	wsID := uuid.New()
	ctx := tenantctx.WithWorkspaceID(context.Background(), wsID)

	var got *uuid.UUID
	if err := env.pool.QueryRow(ctx, "SELECT app_current_workspace_id()").Scan(&got); err != nil {
		t.Fatalf("app_current_workspace_id (wsID bound): %v", err)
	}
	if got == nil || *got != wsID {
		t.Fatalf("expected %s, got %v", wsID, got)
	}

	// No workspace on the context — the GUC should clear and
	// the helper should return NULL.
	if err := env.pool.QueryRow(context.Background(), "SELECT app_current_workspace_id()").Scan(&got); err != nil {
		t.Fatalf("app_current_workspace_id (no binding): %v", err)
	}
	if got != nil {
		t.Fatalf("expected NULL, got %s", *got)
	}
}
