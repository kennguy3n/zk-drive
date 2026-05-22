// Package reconciler keeps the denormalized counters on the
// workspaces table (currently storage_used_bytes) in sync with the
// canonical row-level data in the files / file_versions tables.
//
// Why this exists
//
// workspaces.storage_used_bytes is exposed in the workspace list
// API response (api/drive/workspace.go ListWorkspaces) but the
// existing application code does NOT maintain it on every
// upload/delete — the canonical source of truth is
// SUM(files.size_bytes) WHERE deleted_at IS NULL, which is what
// the billing service (internal/billing/repository.GetStorageUsed)
// already uses for quota enforcement. The counter on the
// workspaces row therefore drifts: it starts at 0 on workspace
// creation and stays there forever for every workspace whose
// frontend reads the cheap-lookup counter rather than calling the
// (more expensive) billing summary endpoint.
//
// This package provides a periodic reconciler that recomputes the
// canonical sum and atomically writes it back to the workspaces
// row. The contract is intentionally "eventually consistent":
//
//   - Billing / quota enforcement keeps using the live SUM via
//     billing.GetStorageUsed because quota decisions must be
//     strongly consistent (a workspace that's 1 byte over its
//     quota at upload time must be rejected immediately, not after
//     the next reconciler tick).
//   - Cheap reads (workspace listing, dashboard widgets, etc.)
//     read the denormalized counter, accepting a small lag (a few
//     minutes by default).
//
// The reconciler runs in two shapes:
//
//   - cmd/reconciler: standalone binary suitable for a K8s
//     CronJob. Runs once and exits.
//   - cmd/worker: in-process periodic invocation gated on
//     RECONCILE_INTERVAL_MINUTES (default 60, 0 disables).
//
// Both call into the same Reconcile entrypoint so the behaviour is
// identical regardless of how the reconciler is scheduled.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrWorkspaceMissing is returned by ReconcileWorkspace when the
// workspace row no longer exists at the time of the FOR UPDATE
// lock. This is expected during normal operation: ReconcileAll
// enumerates workspace IDs in one round-trip and then iterates
// them, so a workspace can be deleted between enumeration and the
// per-workspace lock. ReconcileAll filters this sentinel out
// instead of recording it as a real error — there's nothing to
// reconcile for a workspace that no longer exists, and a noisy
// log line per concurrent deletion is not actionable.
var ErrWorkspaceMissing = errors.New("reconciler: workspace missing")

// Summary is the result of a Reconcile run across all workspaces.
// Inspected/logged by the caller; fields are public so cmd/reconciler
// can emit a structured log line and the worker's periodic loop can
// surface counters into metrics later (WS-17).
type Summary struct {
	// Workspaces is the number of workspace rows the reconciler
	// scanned. A workspace with zero files still counts here — it
	// will likely be a no-op (counter is already 0) but it's
	// processed for completeness.
	Workspaces int

	// Updated is the subset of Workspaces whose stored counter did
	// not match the canonical sum and was therefore rewritten. The
	// distinction matters for monitoring: a steady-state
	// production system should converge to Updated == 0 most of
	// the time; a non-zero value is a signal that the
	// upload/delete code paths are drifting from the canonical
	// schema.
	Updated int

	// TotalDriftBytes is the absolute total of |new - old| across
	// every workspace that was updated. Useful for alerting on
	// "the counters are off by more than X gigabytes total" rather
	// than just "N workspaces drifted".
	TotalDriftBytes int64

	// Errors collects per-workspace failures. The reconciler does
	// NOT abort on the first error — it logs the failure and
	// continues so a single sick workspace doesn't block all the
	// others from being reconciled.
	Errors []WorkspaceError
}

// WorkspaceError captures a single per-workspace failure inside a
// Reconcile run so callers can decide whether the run as a whole
// should be treated as failed.
type WorkspaceError struct {
	WorkspaceID uuid.UUID
	Err         error
}

// Reconciler binds a pool and runs the per-workspace recompute
// against it. Holding the pool on a struct (rather than passing it
// to every call) makes the dependency obvious and keeps the
// worker-side periodic loop concise.
type Reconciler struct {
	pool *pgxpool.Pool
}

// New returns a Reconciler that talks to the given pgxpool. The
// pool MUST be authenticated as a role that can both SELECT from
// files and UPDATE workspaces.storage_used_bytes; in production
// this is the same application role the server uses, so the RLS
// bypass branch (app.workspace_id GUC unset → policy bypass)
// applies and the reconciler can read every workspace's files.
//
// RLS dependency: this caller must NOT have a workspace UUID bound
// to the request ctx (no tenantctx.WithWorkspaceID upstream) so
// PrepareConn leaves app.workspace_id unset and the
// app_current_workspace_id() IS NULL branch of the tenant
// isolation policy fires. Same pattern as cmd/migrate and the
// other background workers. If migrations/024_row_level_security
// ever switches from NULL-means-bypass to an explicit bypass
// token, this caller has to be updated in lockstep or it'll
// silently see zero workspaces.
func New(pool *pgxpool.Pool) *Reconciler {
	return &Reconciler{pool: pool}
}

// ReconcileAll iterates every workspace row and updates its
// storage_used_bytes counter to match the canonical sum. Returns a
// Summary with per-workspace error details; the function-level
// error is non-nil only when the run cannot proceed at all (e.g.
// the workspaces enumeration query fails).
func (r *Reconciler) ReconcileAll(ctx context.Context) (Summary, error) {
	if r == nil || r.pool == nil {
		return Summary{}, errors.New("reconciler: nil pool")
	}

	rows, err := r.pool.Query(ctx, `SELECT id FROM workspaces`)
	if err != nil {
		return Summary{}, fmt.Errorf("list workspaces: %w", err)
	}

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Summary{}, fmt.Errorf("scan workspace id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("iterate workspaces: %w", err)
	}

	var sum Summary
	sum.Workspaces = len(ids)
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return sum, ctx.Err()
		default:
		}
		res, err := r.ReconcileWorkspace(ctx, id)
		if err != nil {
			if errors.Is(err, ErrWorkspaceMissing) {
				// Workspace was deleted between the
				// enumeration query and the per-workspace
				// reconcile; treat as a no-op rather than
				// polluting Errors with a phantom failure.
				// Decrement Workspaces so the Summary counts
				// reflect what was actually reconciled.
				sum.Workspaces--
				continue
			}
			sum.Errors = append(sum.Errors, WorkspaceError{WorkspaceID: id, Err: err})
			continue
		}
		if res.Changed {
			sum.Updated++
			sum.TotalDriftBytes += res.absDrift()
		}
	}
	return sum, nil
}

// Result is the per-workspace outcome of ReconcileWorkspace.
type Result struct {
	WorkspaceID uuid.UUID

	// Old is the counter value before the update — what the
	// frontend / API consumer would have seen if it had read the
	// row just before reconciliation ran.
	Old int64

	// New is the canonical sum recomputed from the files table.
	// On a converged workspace this equals Old.
	New int64

	// Changed is true when Old != New and the UPDATE actually
	// modified a row. Used by ReconcileAll to count drift events
	// and to keep TotalDriftBytes meaningful (no-op reconciles
	// don't accumulate drift).
	Changed bool
}

func (r Result) absDrift() int64 {
	d := r.New - r.Old
	if d < 0 {
		return -d
	}
	return d
}

// ReconcileWorkspace recomputes the storage counter for a single
// workspace. Performed in a single transaction:
//
//  1. SELECT ... FOR UPDATE on the workspaces row to serialise
//     against any other reconciler running concurrently and against
//     any future code path that touches storage_used_bytes
//     directly. The row-level lock is held only for the duration of
//     this one workspace's recompute (~milliseconds), so it does
//     not block uploads happening against the files table.
//  2. COALESCE(SUM(size_bytes), 0) over files where workspace_id
//     matches AND deleted_at IS NULL — matches the canonical query
//     in internal/billing/repository.GetStorageUsed.
//  3. UPDATE workspaces SET storage_used_bytes = ... only if the
//     value has actually changed, so steady-state workspaces don't
//     generate dead rows / WAL churn.
//
// The function is idempotent — calling it twice in a row on a
// converged workspace produces (Changed=false, no UPDATE).
func (r *Reconciler) ReconcileWorkspace(ctx context.Context, workspaceID uuid.UUID) (Result, error) {
	if r == nil || r.pool == nil {
		return Result{}, errors.New("reconciler: nil pool")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		// Rollback is safe to call after commit (it returns
		// ErrTxClosed which we discard); we always defer it so
		// any early-return path leaves the lock released.
		_ = tx.Rollback(context.Background())
	}()

	var old int64
	if err := tx.QueryRow(ctx,
		`SELECT storage_used_bytes FROM workspaces WHERE id = $1 FOR UPDATE`,
		workspaceID,
	).Scan(&old); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Race: workspace was deleted after ReconcileAll's
			// enumeration but before we got the row lock. Signal
			// this via a typed sentinel so ReconcileAll can
			// filter it out cleanly.
			return Result{}, ErrWorkspaceMissing
		}
		return Result{}, fmt.Errorf("lock workspace %s: %w", workspaceID, err)
	}

	var canonical int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(size_bytes), 0)::BIGINT
		FROM files
		WHERE workspace_id = $1 AND deleted_at IS NULL
	`, workspaceID).Scan(&canonical); err != nil {
		return Result{}, fmt.Errorf("sum files for workspace %s: %w", workspaceID, err)
	}

	res := Result{WorkspaceID: workspaceID, Old: old, New: canonical}
	if old != canonical {
		if _, err := tx.Exec(ctx,
			`UPDATE workspaces SET storage_used_bytes = $1, updated_at = $2 WHERE id = $3`,
			canonical, time.Now().UTC(), workspaceID,
		); err != nil {
			return Result{}, fmt.Errorf("update workspace %s: %w", workspaceID, err)
		}
		res.Changed = true
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit reconcile for workspace %s: %w", workspaceID, err)
	}
	return res, nil
}
