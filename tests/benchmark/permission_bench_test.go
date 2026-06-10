package benchmark

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/permission"
)

// permissionDepth is the folder-tree depth the spec calls for: a
// resolution at the leaf must walk 10 ancestors to find an inherited
// grant. permissionUsers is the breadth (distinct grantees) that share
// the tree.
const (
	permissionDepth = 10
	permissionUsers = 100
)

// permissionP95Target is an internal latency budget for inherited
// permission resolution. The spec does not fix a number for this case;
// 50ms p95 for a 10-deep uncached ancestry walk is a sane bar at SME
// scale and is reported so regressions are visible.
const permissionP95Target = 50 * time.Millisecond

// BenchmarkPermissionResolutionUncached measures the raw DB cost of
// resolving an inherited permission at a depth-10 leaf with no cache in
// front — the cold-path / cache-miss latency the permission cache exists
// to amortise. It rotates across `permissionUsers` grantees so the
// measurement is not skewed by a single hot row.
func BenchmarkPermissionResolutionUncached(b *testing.B) {
	env := setupBench(b)

	// Build the chain, capturing the ROOT (first folder) so we can grant
	// on it and force a full ancestry walk from the leaf.
	var root *uuid.UUID
	var leaf uuid.UUID
	var parent *uuid.UUID
	for d := 0; d < permissionDepth; d++ {
		f, err := env.folders.Create(env.wsCtx, env.wsID, parent, fmt.Sprintf("lvl-%02d", d), env.ownerID)
		if err != nil {
			b.Fatalf("create folder depth %d: %v", d, err)
		}
		id := f.ID
		if d == 0 {
			root = &id
		}
		parent = &id
		leaf = id
	}

	grantees := make([]uuid.UUID, 0, permissionUsers)
	for u := 0; u < permissionUsers; u++ {
		usr, err := env.users.Create(env.wsCtx, env.wsID,
			fmt.Sprintf("perm+%d+%s@bench.local", u, uuid.NewString()),
			fmt.Sprintf("Perm User %d", u), "benchPassw0rd!", "member")
		if err != nil {
			b.Fatalf("create user %d: %v", u, err)
		}
		if _, err := env.permissions.Grant(env.wsCtx, env.wsID,
			permission.ResourceFolder, *root, permission.GranteeUser, usr.ID,
			permission.RoleViewer, nil); err != nil {
			b.Fatalf("grant user %d: %v", u, err)
		}
		grantees = append(grantees, usr.ID)
	}

	rec := &latencyRecorder{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g := grantees[i%len(grantees)]
		start := time.Now()
		ok, err := env.permissions.HasAccessWithInheritance(env.wsCtx, env.wsID,
			permission.ResourceFolder, leaf, permission.GranteeUser, g, permission.RoleViewer)
		rec.record(time.Since(start))
		if err != nil {
			b.Fatalf("resolve: %v", err)
		}
		if !ok {
			b.Fatalf("expected inherited access for grantee %s at leaf %s", g, leaf)
		}
	}
	b.StopTimer()
	rec.reportPercentiles(b, permissionP95Target)
	b.ReportMetric(float64(permissionDepth), "tree-depth")
	b.ReportMetric(float64(permissionUsers), "users")
}
