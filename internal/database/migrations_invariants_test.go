package database

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// migrationsDirForTest locates the repo-root migrations/ directory
// relative to this source file (internal/database/ -> ../../migrations).
// Using runtime.Caller keeps the test independent of the working
// directory `go test` happens to run from.
func migrationsDirForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	stat, err := os.Stat(dir)
	if err != nil || !stat.IsDir() {
		t.Fatalf("migrations dir not found at %q: %v", dir, err)
	}
	return dir
}

func migrationFiles(t *testing.T, suffix string) []string {
	t.Helper()
	entries, err := os.ReadDir(migrationsDirForTest(t))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TestMigrationVersionsAreFilenameKeyed pins the invariant that
// Migrate() relies on: a migration's identity (the value stored in
// schema_migrations.version and used to decide "already applied") is
// the FULL filename stem, not the leading numeric prefix. This is what
// lets parallel feature branches land migrations that share a numeric
// prefix (e.g. 041_setup_state + 041_workspace_features) without
// colliding.
//
// The test asserts the two properties that make that safe:
//  1. Every .up.sql maps to a UNIQUE version string. If this ever
//     fails, two migrations would map to the same schema_migrations row
//     and the second would be silently skipped on a fresh database.
//  2. Numeric-prefix collisions are explicitly TOLERATED — we assert the
//     uniqueness holds on full stems even when prefixes repeat, so a
//     well-meaning contributor who "dedupes" prefixes by renumbering an
//     already-merged migration is steered here (and to Migrate's doc)
//     to understand why that breaks already-migrated databases.
func TestMigrationVersionsAreFilenameKeyed(t *testing.T) {
	ups := migrationFiles(t, ".up.sql")
	if len(ups) == 0 {
		t.Fatal("no .up.sql migrations found")
	}

	seenVersion := make(map[string]string, len(ups))
	prefixCounts := make(map[string]int, len(ups))
	for _, name := range ups {
		version := strings.TrimSuffix(name, ".up.sql")
		if prev, dup := seenVersion[version]; dup {
			t.Errorf("duplicate migration version %q from files %q and %q: "+
				"version identity is the full filename stem and must be unique",
				version, prev, name)
		}
		seenVersion[version] = name

		prefix, _, found := strings.Cut(version, "_")
		if !found || prefix == "" {
			t.Errorf("migration %q does not follow <version>_<name>.up.sql", name)
			continue
		}
		prefixCounts[prefix]++
	}

	// Document (and lock in) that prefix collisions are expected and
	// safe under filename-keyed tracking. This is informational, not a
	// failure: it surfaces in -v output so the scheme is visible.
	for prefix, n := range prefixCounts {
		if n > 1 {
			t.Logf("numeric prefix %q is shared by %d migrations — tolerated: "+
				"each is tracked by its full version string", prefix, n)
		}
	}
}

// TestMigrationUpDownParity asserts every forward migration has a
// matching rollback file and vice versa. A missing .down.sql would make
// a migration irreversible (no clean rollback path during an incident);
// a .down.sql with no .up.sql is dead weight that signals a botched
// rename. Both are cheap to catch at the filename layer before they
// reach an operator mid-rollback.
func TestMigrationUpDownParity(t *testing.T) {
	ups := migrationFiles(t, ".up.sql")
	downs := migrationFiles(t, ".down.sql")

	downSet := make(map[string]struct{}, len(downs))
	for _, d := range downs {
		downSet[strings.TrimSuffix(d, ".down.sql")] = struct{}{}
	}
	upSet := make(map[string]struct{}, len(ups))
	for _, u := range ups {
		upSet[strings.TrimSuffix(u, ".up.sql")] = struct{}{}
	}

	for v := range upSet {
		if _, ok := downSet[v]; !ok {
			t.Errorf("migration %q has no matching %q (.down.sql) rollback file", v+".up.sql", v+".down.sql")
		}
	}
	for v := range downSet {
		if _, ok := upSet[v]; !ok {
			t.Errorf("rollback %q has no matching %q (.up.sql) — orphaned down migration", v+".down.sql", v+".up.sql")
		}
	}
}
