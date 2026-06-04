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
