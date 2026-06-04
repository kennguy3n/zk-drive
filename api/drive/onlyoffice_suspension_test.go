package drive

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/collab"
)

// fakeSuspensionChecker is a minimal middleware.WorkspaceSuspensionChecker
// used to drive ensureNotSuspended through its three branches without a
// database or control plane.
type fakeSuspensionChecker struct {
	suspended bool
	err       error
}

func (f fakeSuspensionChecker) WorkspaceSuspension(_ context.Context, _ uuid.UUID) (bool, string, error) {
	return f.suspended, "nonpayment", f.err
}

// TestEnsureNotSuspended pins the write-boundary suspension gate used by
// the ONLYOFFICE save callback (which runs outside SuspensionGuard):
//   - no checker wired  -> allow (nil), preserving metadata-only wiring
//   - workspace active   -> allow (nil)
//   - workspace suspended-> refuse with ErrWorkspaceSuspended
//   - lookup error       -> fail OPEN (nil), since suspension is an
//     availability control and must not drop a user's edited bytes.
func TestEnsureNotSuspended(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		checker *fakeSuspensionChecker
		wantErr error
	}{
		{name: "no checker wired", checker: nil, wantErr: nil},
		{name: "workspace active", checker: &fakeSuspensionChecker{suspended: false}, wantErr: nil},
		{name: "workspace suspended", checker: &fakeSuspensionChecker{suspended: true}, wantErr: collab.ErrWorkspaceSuspended},
		{name: "lookup error fails open", checker: &fakeSuspensionChecker{err: errors.New("db blip")}, wantErr: nil},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{}
			if tc.checker != nil {
				h.suspension = tc.checker
			}
			err := h.ensureNotSuspended(context.Background(), uuid.New())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ensureNotSuspended err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestEnsureNotSuspendedFailClosed pins the compliance-hold posture: with
// WithSuspensionFailClosed(true) a suspension-lookup error rejects the
// write (returns ErrWorkspaceSuspended) instead of allowing it, so the
// Document Server keeps the bytes and retries when the lookup recovers.
// An active workspace still saves.
func TestEnsureNotSuspendedFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		checker *fakeSuspensionChecker
		wantErr error
	}{
		{name: "lookup error fails closed", checker: &fakeSuspensionChecker{err: errors.New("db blip")}, wantErr: collab.ErrWorkspaceSuspended},
		{name: "workspace active still allowed", checker: &fakeSuspensionChecker{suspended: false}, wantErr: nil},
		{name: "workspace suspended refused", checker: &fakeSuspensionChecker{suspended: true}, wantErr: collab.ErrWorkspaceSuspended},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := (&Handler{}).WithSuspensionChecker(tc.checker).WithSuspensionFailClosed(true)
			err := h.ensureNotSuspended(context.Background(), uuid.New())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ensureNotSuspended (fail-closed) err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestWithOnlyOfficeSaveLimits pins the derived concurrency: the cap is
// budget / per-document, floored at 1, and the semaphore is sized to
// match. Non-positive inputs fall back to the package defaults.
func TestWithOnlyOfficeSaveLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		budget, maxDoc  int64
		wantConcurrency int
		wantMaxDoc      int64
	}{
		{name: "default-equivalent", budget: 256 << 20, maxDoc: 100 << 20, wantConcurrency: 2, wantMaxDoc: 100 << 20},
		{name: "higher budget raises cap", budget: 512 << 20, maxDoc: 50 << 20, wantConcurrency: 10, wantMaxDoc: 50 << 20},
		{name: "budget equals one doc floors to 1", budget: 100 << 20, maxDoc: 100 << 20, wantConcurrency: 1, wantMaxDoc: 100 << 20},
		{name: "non-positive falls back to defaults", budget: 0, maxDoc: 0, wantConcurrency: defaultOnlyOfficeMaxConcurrentSaves, wantMaxDoc: defaultOnlyOfficeMaxDocumentBytes},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := (&Handler{}).WithOnlyOfficeSaveLimits(tc.budget, tc.maxDoc)
			if h.onlyOfficeSaveConcurrency != tc.wantConcurrency {
				t.Errorf("concurrency: got %d, want %d", h.onlyOfficeSaveConcurrency, tc.wantConcurrency)
			}
			if cap(h.onlyOfficeSaveSem) != tc.wantConcurrency {
				t.Errorf("semaphore cap: got %d, want %d", cap(h.onlyOfficeSaveSem), tc.wantConcurrency)
			}
			if h.onlyOfficeMaxDocumentBytes != tc.wantMaxDoc {
				t.Errorf("maxDocumentBytes: got %d, want %d", h.onlyOfficeMaxDocumentBytes, tc.wantMaxDoc)
			}
		})
	}
}
