package health

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// fakeStorageDashProbe stands in for the *storage.Client behind a
// StorageDashboardProbe so the probe's traffic-light logic can be
// exercised without the AWS SDK. It satisfies storageDashProbe
// (HealthCheck + RecentErrorStats); the package's other fake
// (fakeStorageProbe) only covers the HealthCheck-only StorageChecker.
type fakeStorageDashProbe struct {
	healthErr error
	stats     storage.OpStats
}

func (f *fakeStorageDashProbe) HealthCheck(context.Context) error { return f.healthErr }
func (f *fakeStorageDashProbe) RecentErrorStats() storage.OpStats { return f.stats }

func TestStorageDashboardProbe(t *testing.T) {
	tests := []struct {
		name  string
		probe *fakeStorageDashProbe
		want  Color
	}{
		{
			name:  "healthy with no recent errors is green",
			probe: &fakeStorageDashProbe{stats: storage.OpStats{Total: 100, Errors: 0}},
			want:  ColorGreen,
		},
		{
			name:  "unreachable gateway is red",
			probe: &fakeStorageDashProbe{healthErr: errors.New("HeadBucket: dial tcp: connection refused")},
			want:  ColorRed,
		},
		{
			// >= storageErrorRateThreshold (0.1) with a live HeadBucket
			// is degraded, not down.
			name:  "elevated error rate over the window is yellow",
			probe: &fakeStorageDashProbe{stats: storage.OpStats{Total: 100, Errors: 25}},
			want:  ColorYellow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newStorageDashboardProbeWithProbe(tc.probe)
			got := p.Probe(context.Background())
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q", got.Status, tc.want)
			}
		})
	}
}

func TestStorageDashboardProbeUnconfigured(t *testing.T) {
	// A nil client (S3 unset) normalises to a nil probe field, which the
	// dashboard reports as unconfigured/unknown rather than red.
	p := NewStorageDashboardProbe(nil)
	got := p.Probe(context.Background())
	if got.Status != ColorUnknown {
		t.Fatalf("status = %q, want %q", got.Status, ColorUnknown)
	}
}
