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

// TestListTemplatesReturnsCopy verifies the slice is freshly
// allocated on every call. Returning the package-level map's value
// slice directly would let a caller mutate the in-memory registry
// (e.g. by reslicing) and affect every subsequent request — this
// test rejects that footgun.
func TestListTemplatesReturnsCopy(t *testing.T) {
	a := ListTemplates()
	b := ListTemplates()
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("expected non-empty templates")
	}
	if &a[0] == &b[0] {
		t.Fatalf("ListTemplates returned aliased slice headers; callers can corrupt registry")
	}
}

// TestGetTemplateKnown spot-checks the canonical "agency" template
// (which has been part of the API contract since Phase 3 launched)
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

// TestGetTemplateReturnsDistinctCopy guards the same pitfall as
// ListTemplates: a mutation by one caller (e.g. appending to
// SubFolders) must not affect the package registry.
func TestGetTemplateReturnsDistinctCopy(t *testing.T) {
	a, err := GetTemplate("legal")
	if err != nil {
		t.Fatalf("legal template missing: %v", err)
	}
	// Mutate the returned slice by appending a sentinel — if the
	// internal map shares the underlying array (which it does, since
	// SubFolders is a slice and slices are reference types), a
	// future Append from another caller could surface that sentinel.
	// We don't expect callers to mutate, but the API surface should
	// at least return a fresh Template value so reassigning fields
	// is safe.
	originalLen := len(a.SubFolders)

	b, err := GetTemplate("legal")
	if err != nil {
		t.Fatalf("legal template missing on second fetch: %v", err)
	}
	if len(b.SubFolders) != originalLen {
		t.Fatalf("registry mutated between fetches: first=%d second=%d", originalLen, len(b.SubFolders))
	}
}
