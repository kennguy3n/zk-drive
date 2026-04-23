// Package permission implements per-resource access grants within a
// workspace. Phase 1 enforces workspace-level role gating (admin vs member)
// at the handler layer; the service here lets callers grant, revoke, list,
// and check individual viewer/editor/admin grants on a single folder or
// file. Folder-level inheritance is deferred to Phase 2.
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
const (
	ResourceFolder = "folder"
	ResourceFile   = "file"
)

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
// type strings.
func isValidResourceType(t string) bool {
	return t == ResourceFolder || t == ResourceFile
}

// isValidGranteeType reports whether t is one of the recognized grantee
// type strings.
func isValidGranteeType(t string) bool {
	return t == GranteeUser || t == GranteeGuest
}
