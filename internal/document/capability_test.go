package document

import (
	"testing"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// TestResolveCapability_ManagedEncrypted pins the capability matrix
// for managed_encrypted folders. This is the "rich + presence"
// ceiling; future modes should fall between this and strict_zk.
func TestResolveCapability_ManagedEncrypted(t *testing.T) {
	t.Parallel()
	got := ResolveCapability(folder.EncryptionManagedEncrypted)
	want := Capability{
		ServerSnapshotAllowed: true,
		RichExtensionsAllowed: true,
		PresenceAllowed:       true,
	}
	if got != want {
		t.Fatalf("managed_encrypted capability mismatch: got %+v, want %+v", got, want)
	}
}

// TestResolveCapability_StrictZK pins the strict-zk "floor". The
// privacy boundary is the folder; strict_zk forbids the server from
// reading content so server-side merges, rich extensions, and
// awareness routing are all OFF.
func TestResolveCapability_StrictZK(t *testing.T) {
	t.Parallel()
	got := ResolveCapability(folder.EncryptionStrictZK)
	want := Capability{
		ServerSnapshotAllowed: false,
		RichExtensionsAllowed: false,
		PresenceAllowed:       false,
	}
	if got != want {
		t.Fatalf("strict_zk capability mismatch: got %+v, want %+v", got, want)
	}
}

// TestResolveCapability_UnknownFailsClosed proves a future
// encryption mode added without updating this resolver gets the
// conservative all-off capability rather than silently leaking
// server-decrypt features.
func TestResolveCapability_UnknownFailsClosed(t *testing.T) {
	t.Parallel()
	got := ResolveCapability("hypothetical_future_mode")
	want := Capability{}
	if got != want {
		t.Fatalf("unknown mode should be all-off, got %+v", got)
	}
	if got2 := ResolveCapability(""); got2 != want {
		t.Fatalf("empty mode should be all-off, got %+v", got2)
	}
}

// TestAllowedCollabModesFor checks the user-selectable mode lists
// emitted to the frontend. Managed_encrypted offers the full set;
// strict_zk caps at markdown only.
func TestAllowedCollabModesFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode string
		want []string
	}{
		{folder.EncryptionManagedEncrypted, []string{CollabModeMarkdown, CollabModeRich, CollabModeRichPresence}},
		{folder.EncryptionStrictZK, []string{CollabModeMarkdown}},
		{"unknown_mode", []string{CollabModeMarkdown}},
		{"", []string{CollabModeMarkdown}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			got := AllowedCollabModesFor(tc.mode)
			if !equalStringsOrdered(got, tc.want) {
				t.Fatalf("AllowedCollabModesFor(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// TestIsCollabModeAllowed enumerates the (folder mode, collab mode)
// matrix to catch any drift between the policy table and the
// per-row check used by the service.
func TestIsCollabModeAllowed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		encMode, collabMode string
		want                bool
	}{
		{folder.EncryptionManagedEncrypted, CollabModeMarkdown, true},
		{folder.EncryptionManagedEncrypted, CollabModeRich, true},
		{folder.EncryptionManagedEncrypted, CollabModeRichPresence, true},
		{folder.EncryptionManagedEncrypted, CollabModeDisabled, false},
		{folder.EncryptionStrictZK, CollabModeMarkdown, true},
		{folder.EncryptionStrictZK, CollabModeRich, false},
		{folder.EncryptionStrictZK, CollabModeRichPresence, false},
		{folder.EncryptionStrictZK, CollabModeDisabled, false},
		// Unknown encryption mode falls back to markdown-only.
		{"future_mode", CollabModeMarkdown, true},
		{"future_mode", CollabModeRich, false},
	}
	for _, tc := range cases {
		t.Run(tc.encMode+"/"+tc.collabMode, func(t *testing.T) {
			got := IsCollabModeAllowed(tc.encMode, tc.collabMode)
			if got != tc.want {
				t.Fatalf("IsCollabModeAllowed(%q, %q) = %v, want %v",
					tc.encMode, tc.collabMode, got, tc.want)
			}
		})
	}
}

// TestDefaultCollabModeFor checks the auto-pick. Managed gets the
// richest mode (matches Google-Docs expectations), strict caps to
// markdown so users don't pick a mode the folder can't honour.
func TestDefaultCollabModeFor(t *testing.T) {
	t.Parallel()
	if got := DefaultCollabModeFor(folder.EncryptionManagedEncrypted); got != CollabModeRichPresence {
		t.Fatalf("managed default = %q, want %q", got, CollabModeRichPresence)
	}
	if got := DefaultCollabModeFor(folder.EncryptionStrictZK); got != CollabModeMarkdown {
		t.Fatalf("strict default = %q, want %q", got, CollabModeMarkdown)
	}
	if got := DefaultCollabModeFor("unknown"); got != CollabModeMarkdown {
		t.Fatalf("unknown default = %q, want %q", got, CollabModeMarkdown)
	}
}

// TestAllCollabModes_StableOrdering pins the canonical mode list
// order so an OpenAPI consumer or frontend that depends on the
// position notices a reorder. The list is intentionally ordered
// least-to-most-feature-rich with 'disabled' last as the tombstone.
func TestAllCollabModes_StableOrdering(t *testing.T) {
	t.Parallel()
	got := AllCollabModes()
	want := []string{CollabModeMarkdown, CollabModeRich, CollabModeRichPresence, CollabModeDisabled}
	if len(got) != len(want) {
		t.Fatalf("AllCollabModes() returned %d modes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllCollabModes()[%d] = %q, want %q (full slice: %v)", i, got[i], want[i], got)
		}
	}
}

// TestIsValidCollabMode_ExhaustiveOverConstants enforces that every
// CollabMode* constant is included in IsValidCollabMode. A new
// constant added without updating allCollabModes will fail here.
func TestIsValidCollabMode_ExhaustiveOverConstants(t *testing.T) {
	t.Parallel()
	for _, m := range []string{CollabModeMarkdown, CollabModeRich, CollabModeRichPresence, CollabModeDisabled} {
		if !IsValidCollabMode(m) {
			t.Fatalf("expected IsValidCollabMode(%q)=true", m)
		}
	}
	if IsValidCollabMode("not-a-mode") {
		t.Fatal("IsValidCollabMode should reject unknown")
	}
	if IsValidCollabMode("") {
		t.Fatal("IsValidCollabMode should reject empty")
	}
}

// equalStringsOrdered compares two string slices element-by-element
// in their existing order. Used for AllowedCollabModesFor (radio-
// button order matters) where a sort-then-compare would mask a
// position drift.
func equalStringsOrdered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

