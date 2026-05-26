package search

import (
	"strings"
	"testing"
)

// TestSearchSQL_TrigramContentArmIsPartialIndexFriendly pins the
// content_text arm of the trgm_files CTE to the exact shape the
// planner needs in order to use the partial GIN index
// idx_files_trgm_content (created WITH the partial predicate
// `WHERE deleted_at IS NULL AND content_text IS NOT NULL`).
//
// The pre-fix arm was
//
//	immutable_unaccent($2) <% immutable_unaccent(COALESCE(f.content_text, ''))
//
// which the planner could NOT match against the partial index for
// TWO reasons:
//
//  1. The query did not assert `content_text IS NOT NULL`, so the
//     planner could not prove the partial-index predicate held.
//  2. The COALESCE wrapped the expression in a CASE that no longer
//     matched the index expression byte-for-byte (even after we
//     dropped COALESCE from the index itself).
//
// The fix is to (a) move `content_text IS NOT NULL` into the WHERE
// clause and (b) drop the COALESCE from BOTH the query and the
// index, so both sides spell the indexed expression identically.
// This test enforces that the SQL still has both properties — any
// later refactor that re-introduces the COALESCE or drops the IS
// NOT NULL filter will regress to a workspace-scoped seq scan and
// this test will catch it.
func TestSearchSQL_TrigramContentArmIsPartialIndexFriendly(t *testing.T) {
	// Must assert content_text IS NOT NULL alongside the <% predicate.
	const wantPredicate = "f.content_text IS NOT NULL AND immutable_unaccent($2) <% immutable_unaccent(f.content_text)"
	if !strings.Contains(searchSQL, wantPredicate) {
		t.Fatalf("trgm_files content arm is missing the partial-index-friendly predicate %q; SQL:\n%s",
			wantPredicate, searchSQL)
	}
	// Must NOT spell the indexed column as COALESCE(content_text, '')
	// anywhere a trigram <% lives — that defeats partial-index match.
	for _, badPattern := range []string{
		"<% immutable_unaccent(COALESCE(f.content_text",
		"<% immutable_unaccent(COALESCE(content_text",
	} {
		if strings.Contains(searchSQL, badPattern) {
			t.Errorf("trgm word_similarity arm still uses COALESCE on content_text (%q) which defeats the partial GIN index; SQL:\n%s",
				badPattern, searchSQL)
		}
	}
}

// TestSearchSQL_FTSUsesExplicitRegconfigCast pins the regconfig
// casts in the FTS files / folders CTEs to match the index
// expression byte-for-byte. The migration 032 indexes are created
// as `to_tsvector('simple'::regconfig, ...)`; the planner can fold
// the implicit and explicit casts together for index matching, but
// the version-portability of that fold has historically been
// fragile. Pinning the casts on both sides removes the foot-gun.
func TestSearchSQL_FTSUsesExplicitRegconfigCast(t *testing.T) {
	// Both the file FTS and folder FTS CTEs must use ::regconfig.
	if !strings.Contains(searchSQL, "to_tsvector('__LANG__'::regconfig") {
		t.Fatalf("FTS to_tsvector calls do not use ::regconfig cast; SQL:\n%s", searchSQL)
	}
	if !strings.Contains(searchSQL, "plainto_tsquery('__LANG__'::regconfig") {
		t.Fatalf("FTS plainto_tsquery calls do not use ::regconfig cast; SQL:\n%s", searchSQL)
	}
}

// TestResolvedLanguage_FallsBackOnUnknown verifies that an unknown
// language string passed through Options falls back to the default
// instead of being substituted into the SQL — the SQL substitution
// is unparameterised so a bad regconfig name would either 500 the
// query OR (worse) allow an injection if the allow-list check were
// ever removed. The fallback path is the safety net we depend on.
func TestResolvedLanguage_FallsBackOnUnknown(t *testing.T) {
	cases := map[string]string{
		"":                          "simple", // empty → default
		"english":                   "english",
		"french":                    "french",
		"klingon":                   "simple", // not in allow-list
		"english; DROP TABLE users": "simple", // injection attempt → default
	}
	for in, want := range cases {
		got := (Options{Language: in}).resolvedLanguage()
		if got != want {
			t.Errorf("resolvedLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMaxOffsetBoundsAreReasonable is a defence-in-depth check that
// MaxOffset stays in the "comfortably absurd" range — high enough
// that no realistic paginated UI hits the cap, low enough that the
// 5-CTE candidate-set blowup at MaxLimit doesn't pin the worker.
// If a future PR raises MaxOffset, the same PR should rethink
// whether keyset pagination is now warranted.
func TestMaxOffsetBoundsAreReasonable(t *testing.T) {
	if MaxOffset < 1_000 {
		t.Errorf("MaxOffset=%d is too small; legitimate paginators need >= 1000", MaxOffset)
	}
	if MaxOffset > 100_000 {
		t.Errorf("MaxOffset=%d is too large; 5*(limit*4 + offset) candidate-set blowup risks pinning the worker", MaxOffset)
	}
}
