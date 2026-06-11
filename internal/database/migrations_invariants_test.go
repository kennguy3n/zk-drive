package database

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// migrationFilePattern is the canonical naming contract for every file
// in migrations/: a zero-padded 3-digit version prefix, an underscore,
// a lower-snake-case name, and a .up.sql / .down.sql suffix. Migrate()
// derives schema_migrations.version from the stem, so a malformed name
// (wrong padding, uppercase, stray characters) would either sort into
// the wrong apply position or produce a surprising version string.
var migrationFilePattern = regexp.MustCompile(`^([0-9]{3})_[a-z0-9_]+\.(up|down)\.sql$`)

// migrationsDirForTest locates the repo-root migrations/ directory
// relative to this source file (internal/database/ -> ../../migrations).
// Using runtime.Caller keeps the test independent of the working
// directory `go test` happens to run from. (If this repo ever moves to
// a hermetic build that relocates the test binary, switch to
// go:embed migrations/* and drop this helper.)
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

// migrationFileNames returns every entry in migrations/ (both
// directions), sorted, so callers can validate the directory as a whole.
func migrationFileNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(migrationsDirForTest(t))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TestMigrationFilenamesWellFormed asserts every file in migrations/
// matches the <NNN>_<lower_snake>.{up,down}.sql contract. Unlike the
// stem-uniqueness check (which the filesystem makes structurally
// impossible to violate), this guard is genuinely falsifiable: a
// migration committed as `41_foo.up.sql`, `041-foo.up.sql`, or
// `041_Foo.up.SQL` fails here, before it lands and silently sorts into
// the wrong apply order or skews the prefix scheme.
func TestMigrationFilenamesWellFormed(t *testing.T) {
	names := migrationFileNames(t)
	if len(names) == 0 {
		t.Fatal("no migration files found")
	}
	for _, name := range names {
		if !migrationFilePattern.MatchString(name) {
			t.Errorf("migration %q does not match %s — expected e.g. 041_workspace_features.up.sql",
				name, migrationFilePattern.String())
		}
	}
}

// TestMigrationVersionsAreFilenameKeyed pins the invariant that
// Migrate() relies on: a migration's identity (the value stored in
// schema_migrations.version and used to decide "already applied") is
// the FULL filename stem, not the leading numeric prefix. This is what
// lets parallel feature branches land migrations that share a numeric
// prefix (e.g. 041_setup_state + 041_workspace_features) without
// colliding.
//
// Two properties make that safe and are asserted here:
//  1. Stem uniqueness within each direction. A directory can't hold two
//     identically-named files, so this is documentation of intent (it
//     steers a contributor who "dedupes" prefixes by renumbering a
//     merged migration here, and to Migrate's doc, to learn why that
//     re-runs an already-applied body and breaks live databases).
//  2. The .up and .down stems form the SAME set — every forward
//     migration is reversible and no rollback is orphaned. This one is
//     falsifiable: add an .up without its .down (or vice versa) and it
//     fails. It folds the old separate parity check into the version
//     space so "version identity" and "reversibility" are pinned
//     together.
func TestMigrationVersionsAreFilenameKeyed(t *testing.T) {
	ups := stemSet(t, ".up.sql")
	downs := stemSet(t, ".down.sql")

	if len(ups) == 0 {
		t.Fatal("no .up.sql migrations found")
	}

	// (1) Document the filename-keyed scheme and surface tolerated
	// prefix collisions in -v output.
	prefixCounts := make(map[string]int, len(ups))
	for stem := range ups {
		prefix, _, found := strings.Cut(stem, "_")
		if !found || prefix == "" {
			t.Errorf("migration stem %q does not contain a <version>_<name> separator", stem)
			continue
		}
		prefixCounts[prefix]++
	}
	for prefix, n := range prefixCounts {
		if n > 1 {
			t.Logf("numeric prefix %q is shared by %d migrations — tolerated: "+
				"each is tracked by its full version string", prefix, n)
		}
	}

	// (2) up and down version spaces must be identical sets.
	for stem := range ups {
		if _, ok := downs[stem]; !ok {
			t.Errorf("migration %q has no matching %q rollback file", stem+".up.sql", stem+".down.sql")
		}
	}
	for stem := range downs {
		if _, ok := ups[stem]; !ok {
			t.Errorf("rollback %q has no matching %q forward migration", stem+".down.sql", stem+".up.sql")
		}
	}
}

// stemSet reads migrations/ and returns the set of version stems for
// files with the given suffix (e.g. "041_workspace_features" for
// ".up.sql"). Because os.ReadDir yields unique names, a duplicate stem
// is impossible — the map is the natural representation of the version
// space for set comparison.
func stemSet(t *testing.T, suffix string) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	for _, name := range migrationFileNames(t) {
		if strings.HasSuffix(name, suffix) {
			out[strings.TrimSuffix(name, suffix)] = struct{}{}
		}
	}
	return out
}
