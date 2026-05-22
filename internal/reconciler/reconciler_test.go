package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestResultAbsDrift exercises the unsigned-distance helper that
// powers Summary.TotalDriftBytes. The reconciler reports drift as a
// magnitude regardless of direction (counter was high or low), so
// the helper must collapse negative deltas to their absolute value
// without overflow on int64 boundaries.
func TestResultAbsDrift(t *testing.T) {
	cases := []struct {
		name        string
		old         int64
		newVal      int64
		wantAbsolut int64
	}{
		{"no_drift", 100, 100, 0},
		{"counter_low", 50, 200, 150},
		{"counter_high", 500, 200, 300},
		{"zero_to_one", 0, 1, 1},
		{"one_to_zero", 1, 0, 1},
		{"negative_zero", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Result{Old: tc.old, New: tc.newVal}
			got := r.absDrift()
			if got != tc.wantAbsolut {
				t.Fatalf("absDrift(old=%d new=%d) = %d, want %d", tc.old, tc.newVal, got, tc.wantAbsolut)
			}
		})
	}
}

// TestReconcileAllRejectsNilPool guards the documented invariant
// that ReconcileAll fails fast rather than panicking when the
// reconciler was constructed with a nil pool — important because
// the standalone binary and the worker both wrap construction in a
// goroutine where a panic would crash the whole process.
func TestReconcileAllRejectsNilPool(t *testing.T) {
	var r *Reconciler
	if _, err := r.ReconcileAll(context.Background()); err == nil {
		t.Fatalf("expected error on nil receiver, got nil")
	}

	r2 := &Reconciler{pool: nil}
	_, err := r2.ReconcileAll(context.Background())
	if err == nil {
		t.Fatalf("expected error on nil pool, got nil")
	}
}

// TestReconcileWorkspaceRejectsNilPool covers the per-workspace
// entrypoint as well — Reconciler exposes both ReconcileAll and
// ReconcileWorkspace publicly, so both need the guard.
func TestReconcileWorkspaceRejectsNilPool(t *testing.T) {
	var r *Reconciler
	if _, err := r.ReconcileWorkspace(context.Background(), uuid.New()); err == nil {
		t.Fatalf("expected error on nil receiver, got nil")
	}

	r2 := &Reconciler{pool: nil}
	if _, err := r2.ReconcileWorkspace(context.Background(), uuid.New()); err == nil {
		t.Fatalf("expected error on nil pool, got nil")
	}
}

// TestSummaryErrorAccumulation is a documentation test that
// captures the contract: WorkspaceError values are accumulated by
// ReconcileAll without aborting the run, so callers can decide
// whether a non-empty Errors slice should flip their exit code.
// (The actual accumulation behaviour is exercised by the
// integration tests against a real database; this test just locks
// in the public shape so future refactors don't change it
// silently.)
func TestSummaryErrorAccumulation(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()
	sum := Summary{
		Workspaces: 3,
		Updated:    1,
		Errors: []WorkspaceError{
			{WorkspaceID: id1, Err: errors.New("boom")},
			{WorkspaceID: id2, Err: errors.New("bang")},
		},
	}
	if len(sum.Errors) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(sum.Errors))
	}
	if sum.Errors[0].WorkspaceID != id1 || sum.Errors[1].WorkspaceID != id2 {
		t.Fatalf("WorkspaceID ordering changed: %+v", sum.Errors)
	}
}
