package permission

import (
	"testing"

	"github.com/kennguy3n/zk-drive/internal/metrics"
)

// TestDBOpLabelsMirrorMetricsPackage pins the equality of the
// per-method op-label string constants between this package
// (where they are consumed by observeQuery) and
// internal/metrics/db.go (where they are documented as the
// canonical names operators see in dashboards).
//
// The strings MUST be duplicated, not shared via import, because
// internal/permission cannot import internal/metrics — the
// dependency direction is metrics-defines-observer-interface,
// permission-calls-observer. An import would either reverse that
// arrow or create an import cycle. So both sites maintain their
// own copy, and this test pins them locked-step.
//
// If either constant drifts (e.g. a refactor renames
// DBOpPermissionCheckAccess to DBOpPermCheckAccess and someone
// forgets the permission side, or vice versa), the metrics
// surface would silently split into two distinct label families
// — dashboards built against the old name would go dark while
// new ones used the new name. The split would survive lint,
// compile, and runtime; only a manual Prometheus query would
// surface it. This pin closes that gap at compile-time.
//
// The doc comment in repository.go:96 had previously claimed "the
// test in cache_test.go pins the canonical strings" but no such
// test existed. This file backs that claim.
func TestDBOpLabelsMirrorMetricsPackage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		permLabel   string
		metricLabel string
	}{
		{
			name:        "check_access",
			permLabel:   dbOpCheckAccess,
			metricLabel: metrics.DBOpPermissionCheckAccess,
		},
		{
			name:        "check_access_with_inheritance",
			permLabel:   dbOpCheckAccessWithInheritance,
			metricLabel: metrics.DBOpPermissionCheckAccessWithInheritance,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.permLabel != tc.metricLabel {
				t.Errorf("permission.%s = %q drift from metrics.%s = %q. "+
					"These constants must stay locked-step — see "+
					"internal/permission/repository.go:91-97 for the "+
					"import-cycle rationale forcing duplication.",
					tc.name, tc.permLabel, tc.name, tc.metricLabel)
			}
		})
	}
}
