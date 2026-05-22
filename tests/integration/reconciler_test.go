package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/reconciler"
)

// TestReconcilerCorrectsStorageDrift exercises the production drift
// scenario the reconciler exists to fix: upload paths add rows to
// the files table but do NOT increment workspaces.storage_used_bytes
// (the canonical source is SUM(files.size_bytes) which billing reads
// directly). After uploads, the workspace counter still reads 0;
// after ReconcileAll runs, it should match the canonical sum.
//
// The test deliberately uses the real API for the file upload step
// (so any future change that DOES start maintaining the counter
// inline will still pass — the reconciler will just become a no-op
// at this step) and then directly probes both the canonical SUM and
// the workspaces row to assert the drift / convergence behaviour.
func TestReconcilerCorrectsStorageDrift(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Resolve workspace ID from the workspace listing — the test
	// harness doesn't expose it directly.
	wsID := readWorkspaceID(t, env, tok.Token)

	fold := createFolder(t, env, tok.Token, nil, "Docs")
	confirmUploadHelper(t, env, tok.Token, fold.ID, "alpha.txt", "text/plain", 4096)
	confirmUploadHelper(t, env, tok.Token, fold.ID, "beta.txt", "text/plain", 8192)

	const expected int64 = 4096 + 8192

	canonical := readCanonicalSum(t, env, wsID)
	if canonical != expected {
		t.Fatalf("canonical SUM(files.size_bytes) = %d, want %d (files-table writes broken?)", canonical, expected)
	}

	if got := readStoredCounter(t, env, wsID); got == expected {
		// The upload path may have been changed in the future to
		// also maintain the counter inline — that's a valid
		// outcome, just means the reconciler becomes a no-op
		// here. Continue to verify ReconcileAll doesn't break
		// anything in that case.
		t.Logf("workspaces.storage_used_bytes already converged to %d before reconcile (upload path now maintains counter inline)", got)
	} else if got != 0 {
		t.Fatalf("workspaces.storage_used_bytes = %d before reconcile; expected 0 (no upload path increments it) or %d (inline maintenance)", got, expected)
	}

	rc := reconciler.New(env.pool)
	res, err := rc.ReconcileWorkspace(context.Background(), wsID)
	if err != nil {
		t.Fatalf("ReconcileWorkspace: %v", err)
	}
	if res.New != expected {
		t.Fatalf("ReconcileWorkspace.New = %d, want %d", res.New, expected)
	}
	if got := readStoredCounter(t, env, wsID); got != expected {
		t.Fatalf("workspaces.storage_used_bytes after reconcile = %d, want %d", got, expected)
	}

	// Calling reconcile a second time on a converged workspace must
	// be a no-op (Changed=false, no UPDATE issued).
	res2, err := rc.ReconcileWorkspace(context.Background(), wsID)
	if err != nil {
		t.Fatalf("second ReconcileWorkspace: %v", err)
	}
	if res2.Changed {
		t.Fatalf("expected Changed=false on second reconcile; got %+v", res2)
	}
	if res2.Old != expected || res2.New != expected {
		t.Fatalf("second ReconcileWorkspace expected Old/New=%d, got Old=%d New=%d", expected, res2.Old, res2.New)
	}
}

// TestReconcilerIgnoresSoftDeletedFiles asserts the reconciler's
// canonical query matches the contract baked into billing.GetStorageUsed:
// only rows with deleted_at IS NULL count. Soft-deleted files must
// not inflate the counter.
func TestReconcilerIgnoresSoftDeletedFiles(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	wsID := readWorkspaceID(t, env, tok.Token)
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	keepID := confirmUploadHelper(t, env, tok.Token, fold.ID, "keep.txt", "text/plain", 1024)
	delID := confirmUploadHelper(t, env, tok.Token, fold.ID, "trash.txt", "text/plain", 2048)

	// Soft-delete the second file directly so we don't depend on
	// the file-delete handler's specific semantics (which might
	// also bookkeep the counter inline in a future change).
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE files SET deleted_at = $1 WHERE id = $2`, time.Now().UTC(), delID,
	); err != nil {
		t.Fatalf("soft-delete file: %v", err)
	}

	rc := reconciler.New(env.pool)
	res, err := rc.ReconcileWorkspace(context.Background(), wsID)
	if err != nil {
		t.Fatalf("ReconcileWorkspace: %v", err)
	}
	if res.New != 1024 {
		t.Fatalf("reconciled value = %d, want 1024 (soft-deleted %s must not count)", res.New, delID)
	}
	_ = keepID
}

// TestReconcilerReconcileAllIsBestEffort exercises the multi-workspace
// path: a fresh harness has exactly one workspace, and ReconcileAll
// should report Workspaces=1 with no errors. Adding more workspaces
// in the same harness is tricky (signup creates new ones) but the
// path is shared with ReconcileWorkspace so it's sufficient to
// assert the wiring works end-to-end and the summary fields are
// populated correctly.
func TestReconcilerReconcileAllIsBestEffort(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	wsID := readWorkspaceID(t, env, tok.Token)

	fold := createFolder(t, env, tok.Token, nil, "Docs")
	confirmUploadHelper(t, env, tok.Token, fold.ID, "only.txt", "text/plain", 2048)

	rc := reconciler.New(env.pool)
	sum, err := rc.ReconcileAll(context.Background())
	if err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if sum.Workspaces < 1 {
		t.Fatalf("expected at least 1 workspace, got %d", sum.Workspaces)
	}
	if len(sum.Errors) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(sum.Errors), sum.Errors)
	}
	// The freshly-uploaded workspace must show up as Updated and
	// account for at least 2048 bytes of drift.
	if sum.Updated < 1 {
		t.Fatalf("expected Updated>=1 (counter started at 0), got %d", sum.Updated)
	}
	if sum.TotalDriftBytes < 2048 {
		t.Fatalf("expected TotalDriftBytes>=2048, got %d", sum.TotalDriftBytes)
	}

	if got := readStoredCounter(t, env, wsID); got != 2048 {
		t.Fatalf("workspaces.storage_used_bytes after ReconcileAll = %d, want 2048", got)
	}
}

// readWorkspaceID hits GET /api/workspaces and returns the single
// workspace ID from the listing. setupEnv + signupAndLogin produce
// exactly one workspace, so the absence of "exactly one" here is a
// test failure not a tolerance.
func readWorkspaceID(t *testing.T, env *testEnv, token string) uuid.UUID {
	t.Helper()
	status, body := env.httpRequest(http.MethodGet, "/api/workspaces", token, nil)
	if status != http.StatusOK {
		t.Fatalf("list workspaces: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Workspaces []struct {
			ID uuid.UUID `json:"id"`
		} `json:"workspaces"`
	}
	env.decodeJSON(body, &resp)
	if len(resp.Workspaces) != 1 {
		t.Fatalf("expected exactly 1 workspace in fresh harness, got %d", len(resp.Workspaces))
	}
	return resp.Workspaces[0].ID
}

// readCanonicalSum runs the same SUM the reconciler does, so the
// test can independently verify what value the reconciler should
// converge on.
func readCanonicalSum(t *testing.T, env *testEnv, workspaceID uuid.UUID) int64 {
	t.Helper()
	var total int64
	if err := env.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(size_bytes), 0)::BIGINT FROM files WHERE workspace_id = $1 AND deleted_at IS NULL`,
		workspaceID,
	).Scan(&total); err != nil {
		t.Fatalf("read canonical sum: %v", err)
	}
	return total
}

// readStoredCounter returns the current value of the workspaces
// row's storage_used_bytes column — the denormalized counter the
// reconciler maintains.
func readStoredCounter(t *testing.T, env *testEnv, workspaceID uuid.UUID) int64 {
	t.Helper()
	var total int64
	if err := env.pool.QueryRow(context.Background(),
		`SELECT storage_used_bytes FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&total); err != nil {
		t.Fatalf("read stored counter: %v", err)
	}
	return total
}
