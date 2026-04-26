// Package kchat owns the KChat ↔ ZK Drive integration surface:
// room-folder mapping, permission sync against folder grants, and
// room-scoped attachment uploads. The package is deliberately
// dependency-light — it interacts with the folder, permission, file
// and storage subsystems through small interfaces (FolderCreator,
// PermissionGranter, etc.) so it stays free of import cycles.
package kchat

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// RoomFolder is the persisted mapping between a KChat room and a ZK
// Drive folder. The folder lives in the regular folders tree so
// every existing endpoint (listing, permissions, search) works
// transparently against it; this row just records the KChat-side
// identifier and the creator for audit purposes.
type RoomFolder struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	KChatRoomID string    `json:"kchat_room_id"`
	FolderID    uuid.UUID `json:"folder_id"`
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// MemberSync is one entry in a room-membership snapshot pushed to
// SyncMembers. Role is one of the permission package's role
// constants (viewer / editor / admin); UserID is a ZK Drive user UUID.
type MemberSync struct {
	UserID uuid.UUID `json:"user_id"`
	Role   string    `json:"role"`
}

// Errors returned by the service. Handlers translate these into HTTP
// status codes; see api/kchat/handler.go for the mapping.
var (
	// ErrInvalidRoomID is returned when a kchat_room_id is empty
	// after trimming.
	ErrInvalidRoomID = errors.New("kchat: room id must not be empty")

	// ErrRoomNotFound is returned when a lookup can't find a row.
	ErrRoomNotFound = errors.New("kchat: room not mapped")

	// ErrRoomAlreadyMapped is returned by Create when the
	// (workspace_id, kchat_room_id) tuple already has a row. Callers
	// can surface this as 409 Conflict.
	ErrRoomAlreadyMapped = errors.New("kchat: room already mapped to a folder")

	// ErrInvalidRole is returned when a member's role is not one of
	// the recognized permission roles.
	ErrInvalidRole = errors.New("kchat: invalid member role")

	// ErrInvalidObjectKey is returned by ConfirmAttachment when the
	// object_key is empty or otherwise malformed by the caller.
	// Handlers map this to 400.
	ErrInvalidObjectKey = errors.New("kchat: object_key is required")

	// ErrInvalidSize is returned when size_bytes is negative on the
	// upload URL or confirm path. Handlers map this to 400.
	ErrInvalidSize = errors.New("kchat: size_bytes must be non-negative")

	// ErrObjectKeyMismatch is returned by ConfirmAttachment when the
	// supplied object_key does not match the workspace + file ID
	// prefix produced by AttachmentUploadURL. Treated as a client
	// error (400) because the caller is presenting a key that did
	// not come from this service.
	ErrObjectKeyMismatch = errors.New("kchat: object_key does not belong to file")
)
