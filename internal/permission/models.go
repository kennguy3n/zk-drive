// Package permission implements per-resource access grants within a
// workspace. The handler layer enforces workspace-level role gating
// (admin vs member); the service here lets callers grant, revoke,
// list, and check individual viewer/editor/admin grants on a single
// folder or file. Folder-level inheritance is a follow-up.
package permission

import (
	"time"

	"github.com/google/uuid"
)

// Role values recognized for a permission grant on a resource.
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleAdmin  = "admin"
)

// ResourceType values recognized for a permission grant.
//
// IMPORTANT: every Resource* constant declared here MUST also be
// listed in AllResourceTypes below. Downstream consumers
// (notably the changefeed cache-invalidation matrix in
// internal/changefeed/service.go's shouldBustForMutation)
// iterate AllResourceTypes to enforce audit coverage. The failure
// mode this guards against: adding a new resource type here
// without updating the bust matrix would leave stale cache
// entries when that resource's container changed — caught by
// TestPermissionResourceTypesCoupleToChangefeedKinds in
// internal/changefeed/permission_coupling_test.go.
const (
	ResourceFolder = "folder"
	ResourceFile   = "file"
)

// AllResourceTypes is the closed enumeration of every valid
// resource type a permission grant may target. This slice is the
// canonical iteration registry — isValidResourceType derives
// from it, and downstream cross-package tests (e.g., the
// changefeed bust-matrix coupling test) consume it to detect
// audit gaps when a new ResourceType is added.
//
// CONTRACT: if you add a new Resource* constant above, you MUST
// append it here too. The companion test
// TestAllResourceTypesIsComplete in models_test.go pins the
// count so the registry can't silently drift from the const
// block.
var AllResourceTypes = []string{ResourceFolder, ResourceFile}

// GranteeType values recognized for a permission grant.
const (
	GranteeUser  = "user"
	GranteeGuest = "guest"
)

// Permission is a per-resource grant scoped to a workspace. ExpiresAt is
// nullable: nil means the grant never expires.
type Permission struct {
	ID           uuid.UUID  `json:"id"`
	WorkspaceID  uuid.UUID  `json:"workspace_id"`
	ResourceType string     `json:"resource_type"`
	ResourceID   uuid.UUID  `json:"resource_id"`
	GranteeType  string     `json:"grantee_type"`
	GranteeID    uuid.UUID  `json:"grantee_id"`
	Role         string     `json:"role"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// roleRank converts a role string into an integer ranking used by the
// hierarchy check (admin > editor > viewer). Unknown roles map to 0 so a
// typo can never accidentally grant access.
func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// isValidRole reports whether role is one of the recognized role strings.
func isValidRole(role string) bool {
	return role == RoleViewer || role == RoleEditor || role == RoleAdmin
}

// isValidResourceType reports whether t is one of the recognized resource
// type strings. Derives from AllResourceTypes so a future Resource*
// addition flows through automatically once the registry is updated
// — no double-edit hazard between the const block, this function,
// and the cross-package coupling test.
func isValidResourceType(t string) bool {
	for _, rt := range AllResourceTypes {
		if t == rt {
			return true
		}
	}
	return false
}

// isValidGranteeType reports whether t is one of the recognized grantee
// type strings.
func isValidGranteeType(t string) bool {
	return t == GranteeUser || t == GranteeGuest
}
