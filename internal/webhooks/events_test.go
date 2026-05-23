package webhooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestFileEventData_OmitsOptionalNilFields pins the contract that
// optional uuid.UUID fields (VersionID, FolderID) are OMITTED from
// the JSON output when not set, rather than serialised as the zero
// UUID. The earlier `uuid.UUID `json:"...,omitempty"` shape silently
// did the wrong thing because Go's encoding/json never considers a
// [16]byte array empty — see the docblock on FileEventData in
// events.go.
func TestFileEventData_OmitsOptionalNilFields(t *testing.T) {
	t.Parallel()
	d := FileEventData{
		FileID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Name:   "report.pdf",
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, "version_id") {
		t.Errorf("expected version_id to be omitted, got: %s", s)
	}
	if strings.Contains(s, "folder_id") {
		t.Errorf("expected folder_id to be omitted, got: %s", s)
	}
	if strings.Contains(s, "00000000-0000-0000-0000-000000000000") {
		t.Errorf("zero UUID leaked into payload: %s", s)
	}
	if !strings.Contains(s, `"file_id":"00000000-0000-0000-0000-000000000001"`) {
		t.Errorf("required file_id missing: %s", s)
	}
}

// TestFileEventData_IncludesOptionalSetFields verifies that when
// VersionID / FolderID ARE set, they appear in the JSON output.
func TestFileEventData_IncludesOptionalSetFields(t *testing.T) {
	t.Parallel()
	ver := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	folder := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	d := FileEventData{
		FileID:    uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		VersionID: &ver,
		FolderID:  &folder,
		Name:      "report.pdf",
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"version_id":"00000000-0000-0000-0000-000000000002"`) {
		t.Errorf("version_id missing: %s", s)
	}
	if !strings.Contains(s, `"folder_id":"00000000-0000-0000-0000-000000000003"`) {
		t.Errorf("folder_id missing: %s", s)
	}
}

// TestPermissionEventData_OmitsNilGranteeID mirrors the FileEventData
// contract for permission.* events: a nil GranteeID (e.g.
// guest-link grant where no user account exists) must be omitted
// from the JSON output, not serialised as the zero UUID.
func TestPermissionEventData_OmitsNilGranteeID(t *testing.T) {
	t.Parallel()
	d := PermissionEventData{
		ResourceType: "file",
		ResourceID:   uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		GranteeEmail: "guest@example.com",
		Role:         "viewer",
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, "grantee_id") {
		t.Errorf("expected grantee_id to be omitted, got: %s", s)
	}
	if strings.Contains(s, "00000000-0000-0000-0000-000000000000") {
		t.Errorf("zero UUID leaked into payload: %s", s)
	}
	if !strings.Contains(s, `"grantee_email":"guest@example.com"`) {
		t.Errorf("grantee_email missing: %s", s)
	}
}

// TestPermissionEventData_IncludesGranteeID verifies that when
// GranteeID is set, it appears in the JSON output.
func TestPermissionEventData_IncludesGranteeID(t *testing.T) {
	t.Parallel()
	gid := uuid.MustParse("00000000-0000-0000-0000-000000000007")
	d := PermissionEventData{
		ResourceType: "folder",
		ResourceID:   uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		GranteeID:    &gid,
		Role:         "editor",
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"grantee_id":"00000000-0000-0000-0000-000000000007"`) {
		t.Errorf("grantee_id missing: %s", s)
	}
}
