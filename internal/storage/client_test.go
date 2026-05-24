package storage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// TestValidateObjectKey exercises the canonical-form contract enforced
// by storage.ValidateObjectKey: the key must be exactly
// "<workspace_uuid>/<file_uuid>/<version_uuid>", scoped to the caller's
// workspace + file, with no path-traversal, null-byte, backslash, or
// empty-segment escape hatches. The table covers the documented attack
// vectors plus a happy-path round-trip with storage.NewObjectKey.
func TestValidateObjectKey(t *testing.T) {
	// Fixture UUIDs deliberately contain hex letters (a-f) so the
	// "uppercase rejected" / "hyphenless rejected" cases exercise a
	// real case-/format-difference rather than rendering identical
	// to the canonical lowercase form.
	workspaceID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	fileID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	versionID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	otherWorkspace := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	otherFile := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")

	tests := []struct {
		name        string
		key         string
		wantOK      bool
		wantVersion uuid.UUID
		why         string
	}{
		{
			name:        "canonical key from NewObjectKey accepted",
			key:         storage.NewObjectKey(workspaceID, fileID, versionID),
			wantOK:      true,
			wantVersion: versionID,
			why:         "round-trips with the producer; if this breaks, every confirm-upload regresses",
		},
		{
			name:   "dotdot in version segment rejected",
			key:    workspaceID.String() + "/" + fileID.String() + "/..",
			wantOK: false,
			why:    "explicit path-traversal token; never appears in a valid UUID",
		},
		{
			name:   "dotdot in middle path segment rejected (HasPrefix bypass)",
			key:    workspaceID.String() + "/../" + otherFile.String() + "/" + versionID.String(),
			wantOK: false,
			why:    "a HasPrefix-only check would let this through if the prefix matched up to ..",
		},
		{
			name:   "dotdot suffix appended to canonical key rejected",
			key:    storage.NewObjectKey(workspaceID, fileID, versionID) + "/../../etc/passwd",
			wantOK: false,
			why:    "canonical-form object_key blocks path traversal",
		},
		{
			name:   "single dot segment rejected",
			key:    workspaceID.String() + "/" + fileID.String() + "/.",
			wantOK: false,
			why:    "current-directory token has no place in canonical form",
		},
		{
			name:   "null byte in key rejected",
			key:    storage.NewObjectKey(workspaceID, fileID, versionID) + "\x00.txt",
			wantOK: false,
			why:    "C-string truncation attack against downstream consumers",
		},
		{
			name:   "embedded null byte before final segment rejected",
			key:    workspaceID.String() + "\x00/" + fileID.String() + "/" + versionID.String(),
			wantOK: false,
			why:    "NUL anywhere short-circuits any string equality on path-aware backends",
		},
		{
			name:   "backslash in key rejected",
			key:    workspaceID.String() + "\\" + fileID.String() + "\\" + versionID.String(),
			wantOK: false,
			why:    "backslash separators differ between S3 implementations; only forward slash is canonical",
		},
		{
			name:   "wrong workspace rejected (cross-tenant)",
			key:    storage.NewObjectKey(otherWorkspace, fileID, versionID),
			wantOK: false,
			why:    "this is the cross-tenant read primitive the validator's strict equality blocks",
		},
		{
			name:   "wrong file rejected (cross-file within same workspace)",
			key:    storage.NewObjectKey(workspaceID, otherFile, versionID),
			wantOK: false,
			why:    "even within a tenant, a client must not confirm against another file's key",
		},
		{
			name:   "non-UUID version segment rejected",
			key:    workspaceID.String() + "/" + fileID.String() + "/not-a-uuid",
			wantOK: false,
			why:    "version slot must be a UUID; arbitrary strings would let clients steer the storage namespace",
		},
		{
			name:   "non-UUID workspace segment rejected",
			key:    "not-a-uuid/" + fileID.String() + "/" + versionID.String(),
			wantOK: false,
			why:    "matches type contract; uuid.Parse acts as a defence-in-depth filter",
		},
		{
			name:   "too few segments rejected",
			key:    workspaceID.String() + "/" + fileID.String(),
			wantOK: false,
			why:    "an incomplete key would otherwise be padded by downstream code and create ambiguous storage paths",
		},
		{
			name:   "too many segments rejected",
			key:    storage.NewObjectKey(workspaceID, fileID, versionID) + "/extra",
			wantOK: false,
			why:    "extra segments are how a traversal payload would be smuggled past a HasPrefix check",
		},
		{
			name:   "empty key rejected",
			key:    "",
			wantOK: false,
			why:    "guards a class of off-by-one mistakes where the client sends an empty body field",
		},
		{
			name:   "leading slash rejected",
			key:    "/" + storage.NewObjectKey(workspaceID, fileID, versionID),
			wantOK: false,
			why:    "absolute-looking key produces an empty leading segment, never canonical",
		},
		{
			name:   "trailing slash rejected",
			key:    storage.NewObjectKey(workspaceID, fileID, versionID) + "/",
			wantOK: false,
			why:    "trailing slash creates an empty fourth segment",
		},
		{
			name:   "double slash inside rejected",
			key:    workspaceID.String() + "//" + versionID.String(),
			wantOK: false,
			why:    "consecutive slashes flatten in some S3 implementations and could re-introduce traversal",
		},
		{
			name:   "uppercase UUID rejected (S3 keys are case-sensitive)",
			key:    strings.ToUpper(storage.NewObjectKey(workspaceID, fileID, versionID)),
			wantOK: false,
			why:    "uuid.Parse accepts mixed case but NewObjectKey always emits lowercase; an uppercase key would round-trip to a different S3 object and 404 on download",
		},
		{
			name:   "urn:uuid prefix on workspace segment rejected",
			key:    "urn:uuid:" + workspaceID.String() + "/" + fileID.String() + "/" + versionID.String(),
			wantOK: false,
			why:    "uuid.Parse accepts urn:uuid: prefix but it is not canonical and creates a different S3 key",
		},
		{
			name:   "braced UUID on file segment rejected",
			key:    workspaceID.String() + "/{" + fileID.String() + "}/" + versionID.String(),
			wantOK: false,
			why:    "uuid.Parse accepts braced form but NewObjectKey never emits braces",
		},
		{
			name:   "hyphenless UUID on version segment rejected",
			key:    workspaceID.String() + "/" + fileID.String() + "/" + strings.ReplaceAll(versionID.String(), "-", ""),
			wantOK: false,
			why:    "uuid.Parse accepts 32-hex-digit form but NewObjectKey always emits the dashed form",
		},
		{
			name:   "uuid.Nil version rejected (insertVersionTx would silently overwrite)",
			key:    workspaceID.String() + "/" + fileID.String() + "/" + uuid.Nil.String(),
			wantOK: false,
			why:    "v.ID == uuid.Nil triggers a fresh uuid.New() in insertVersionTx, defeating the object-key-to-DB-id pinning invariant",
		},
		{
			name:   "uuid.Nil workspace rejected (defense in depth even if caller passes uuid.Nil expected)",
			key:    uuid.Nil.String() + "/" + fileID.String() + "/" + versionID.String(),
			wantOK: false,
			why:    "uuid.Nil is never a real workspace; reject regardless of what the caller supplies as expectedWorkspace",
		},
		{
			name:   "uuid.Nil file rejected",
			key:    workspaceID.String() + "/" + uuid.Nil.String() + "/" + versionID.String(),
			wantOK: false,
			why:    "uuid.Nil is never a real file id; same fail-closed posture as the version segment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := storage.ValidateObjectKey(tt.key, workspaceID, fileID)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected success, got %v (key=%q reason=%s)", err, tt.key, tt.why)
				}
				if got != tt.wantVersion {
					t.Fatalf("version = %s, want %s", got, tt.wantVersion)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %v, got nil (key=%q reason=%s)", storage.ErrInvalidObjectKey, tt.key, tt.why)
			}
			if !errors.Is(err, storage.ErrInvalidObjectKey) {
				t.Fatalf("error %v is not ErrInvalidObjectKey (key=%q)", err, tt.key)
			}
		})
	}
}

// TestValidateObjectKey_ErrorMessageDoesNotLeak guards a subtle
// security property: the validator returns the same sentinel error
// regardless of *which* rule the key violated, so a probing client
// cannot binary-search the canonical shape from error responses.
func TestValidateObjectKey_ErrorMessageDoesNotLeak(t *testing.T) {
	workspaceID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	fileID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	cases := []string{
		"",
		"not-three-segments",
		"foo/bar/baz",
		workspaceID.String() + "/" + fileID.String() + "/..",
		workspaceID.String() + "\x00/" + fileID.String() + "/whatever",
	}
	for _, k := range cases {
		_, err := storage.ValidateObjectKey(k, workspaceID, fileID)
		if err == nil {
			t.Fatalf("expected error for key %q, got nil", k)
		}
		if got := err.Error(); got != storage.ErrInvalidObjectKey.Error() {
			t.Fatalf("leaky error message %q for key %q (want %q)", got, k, storage.ErrInvalidObjectKey.Error())
		}
	}
}
