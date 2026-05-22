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
// (direct + workspaces + version/preview chains).
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
	"retention_policies",
	"file_tags",
	"workspace_plans",
	"usage_events",
	"workspace_storage_credentials",
	"kchat_room_folders",
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
