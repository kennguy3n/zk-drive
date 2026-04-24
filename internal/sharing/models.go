// Package sharing implements share-link and guest-invite primitives that
// let workspaces expose folders or files to unauthenticated visitors
// (share links) or external users by email (guest invites). Share links
// are independent of the user permission model (ARCHITECTURE.md §7.3);
// guest invites create a `permissions` row with grantee_type='guest' and
// an expires_at, so the regular permission machinery handles access
// checks once the invite is accepted.
package sharing

import (
	"time"

	"github.com/google/uuid"
)

// ShareLink grants anyone holding the token access to a folder or file
// within a workspace. PasswordHash, ExpiresAt and MaxDownloads are each
// optional constraints; DownloadCount tracks successful resolutions so
// the server can enforce MaxDownloads.
type ShareLink struct {
	ID            uuid.UUID  `json:"id"`
	WorkspaceID   uuid.UUID  `json:"workspace_id"`
	ResourceType  string     `json:"resource_type"`
	ResourceID    uuid.UUID  `json:"resource_id"`
	Token         string     `json:"token"`
	PasswordHash  *string    `json:"-"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	MaxDownloads  *int       `json:"max_downloads,omitempty"`
	DownloadCount int        `json:"download_count"`
	CreatedBy     uuid.UUID  `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
}

// RequiresPassword reports whether the share link is gated by a
// password. Callers surfacing resolution errors use this to decide
// between "401 password required" and "404 not found".
func (s *ShareLink) RequiresPassword() bool {
	return s.PasswordHash != nil && *s.PasswordHash != ""
}

// IsExpired reports whether the link has passed its expiry timestamp as
// of the supplied now.
func (s *ShareLink) IsExpired(now time.Time) bool {
	return s.ExpiresAt != nil && !s.ExpiresAt.After(now)
}

// IsExhausted reports whether the link has already been downloaded the
// maximum number of times permitted.
func (s *ShareLink) IsExhausted() bool {
	return s.MaxDownloads != nil && s.DownloadCount >= *s.MaxDownloads
}

// GuestInvite records an invitation for an external user (identified by
// email) to collaborate on a folder with a specific role. AcceptedAt is
// set the first time the invite is resolved; the matching
// `permissions` grant is created at invite time with the same
// ExpiresAt so access is revoked automatically when the invite lapses.
type GuestInvite struct {
	ID           uuid.UUID  `json:"id"`
	WorkspaceID  uuid.UUID  `json:"workspace_id"`
	Email        string     `json:"email"`
	FolderID     uuid.UUID  `json:"folder_id"`
	Role         string     `json:"role"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
	PermissionID uuid.UUID  `json:"permission_id"`
	CreatedBy    uuid.UUID  `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Resource type constants mirror the permission package. We duplicate
// them here so the sharing package does not depend on permission just
// to reference the string literals, and to make it obvious which values
// are legal on a share link.
const (
	ResourceFolder = "folder"
	ResourceFile   = "file"
)

// IsValidResourceType reports whether t is one of the recognized
// resource-type strings for a share link.
func IsValidResourceType(t string) bool {
	return t == ResourceFolder || t == ResourceFile
}
