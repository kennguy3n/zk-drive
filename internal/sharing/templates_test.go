package sharing

import (
	"errors"
	"sort"
	"testing"
)

// TestListTemplatesIsSorted pins the contract that callers depend on:
// the slice is sorted by Name ascending so the HTTP response (and any
// client-side cache keyed on the array) is stable across server
// restarts. A future contributor reaching for a `range
// builtinTemplates` walk somewhere else in the codebase will trip
// this test.
func TestListTemplatesIsSorted(t *testing.T) {
	got := ListTemplates()
	if len(got) == 0 {
		t.Fatalf("ListTemplates returned no templates — every release ships at least the five Phase-3 verticals")
	}
	names := make([]string, len(got))
	for i, tmpl := range got {
		names[i] = tmpl.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("ListTemplates not sorted: %v", names)
	}
}

// TestListTemplatesReturnsDeepCopy is the load-bearing pin on the
// "callers cannot corrupt the registry" contract. It actually mutates
// the first template's first SubFolder via the returned value, then
// re-fetches and asserts the registry is unchanged. A shallow copy
// (which is what `tc := t` produces — the SubFolders slice header
// still points at the registry's underlying array) would let the
// mutation flow through to the second fetch and fail this test.
func TestListTemplatesReturnsDeepCopy(t *testing.T) {
	a := ListTemplates()
	if len(a) == 0 {
		t.Fatalf("expected non-empty templates")
	}
	// Outer-slice independence: separate fetches must yield distinct
	// slice headers so an append() on one cannot grow/shrink the
	// other.
	b := ListTemplates()
	if len(b) == 0 {
		t.Fatalf("expected non-empty templates on second fetch")
	}
	if &a[0] == &b[0] {
		t.Fatalf("ListTemplates returned aliased slice headers")
	}
	// Inner-slice independence: mutate a SubFolder via the first
	// fetch and assert the second fetch sees the original value.
	// This is the test the previous tautology pretended to be.
	if len(a[0].SubFolders) == 0 {
		t.Fatalf("first template has no SubFolders to mutate")
	}
	original := a[0].SubFolders[0]
	a[0].SubFolders[0] = "MUTATED-BY-TEST"

	c := ListTemplates()
	if c[0].SubFolders[0] != original {
		t.Fatalf("ListTemplates leaked SubFolders aliasing — mutation via earlier fetch corrupted registry (got %q, want %q)", c[0].SubFolders[0], original)
	}
}

// TestGetTemplateKnown spot-checks the canonical "agency" template
// (which is part of the documented API contract)
// to catch accidental edits to SubFolders ordering — the UI surfaces
// folders in this exact order.
func TestGetTemplateKnown(t *testing.T) {
	tmpl, err := GetTemplate("agency")
	if err != nil {
		t.Fatalf("agency template missing: %v", err)
	}
	if tmpl.Name != "agency" {
		t.Fatalf("got name %q, want %q", tmpl.Name, "agency")
	}
	wantSubFolders := []string{"Briefs", "Assets", "Deliverables", "Feedback"}
	if len(tmpl.SubFolders) != len(wantSubFolders) {
		t.Fatalf("agency SubFolders length=%d want %d", len(tmpl.SubFolders), len(wantSubFolders))
	}
	for i, want := range wantSubFolders {
		if tmpl.SubFolders[i] != want {
			t.Fatalf("SubFolders[%d]=%q, want %q (order matters for UI rendering)", i, tmpl.SubFolders[i], want)
		}
	}
}

// TestGetTemplateUnknown maps to the API contract: callers expect a
// concrete sentinel error so they can render a clean 404/400 rather
// than a generic 500.
func TestGetTemplateUnknown(t *testing.T) {
	_, err := GetTemplate("nonexistent")
	if !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("expected ErrUnknownTemplate, got %v", err)
	}
}

// TestGetTemplateReturnsDeepCopy guards the same pitfall as
// ListTemplates but on the single-template path. The previous
// version of this test was a tautology — it observed length without
// mutating, so a shallow copy in GetTemplate would pass. Here we
// actually overwrite the first SubFolder via the returned pointer
// and assert the second fetch sees the original value.
func TestGetTemplateReturnsDeepCopy(t *testing.T) {
	a, err := GetTemplate("legal")
	if err != nil {
		t.Fatalf("legal template missing: %v", err)
	}
	if len(a.SubFolders) == 0 {
		t.Fatalf("legal template has no SubFolders to mutate")
	}
	original := a.SubFolders[0]
	a.SubFolders[0] = "MUTATED-BY-TEST"

	b, err := GetTemplate("legal")
	if err != nil {
		t.Fatalf("legal template missing on second fetch: %v", err)
	}
	if b.SubFolders[0] != original {
		t.Fatalf("GetTemplate leaked SubFolders aliasing — mutation via earlier fetch corrupted registry (got %q, want %q)", b.SubFolders[0], original)
	}

	// Outer struct must also be a fresh allocation so a caller doing
	// `tpl.Name = "..."` cannot rename the registry entry.
	if a == b {
		t.Fatalf("GetTemplate returned the same pointer twice; callers can rename templates in place")
	}
}
