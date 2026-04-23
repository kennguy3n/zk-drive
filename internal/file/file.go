package file

import (
	"time"

	"github.com/google/uuid"
)

// File is the logical identity of a document; content lives in zk-object-fabric
// and is referenced through FileVersion rows.
type File struct {
	ID               uuid.UUID  `json:"id"`
	WorkspaceID      uuid.UUID  `json:"workspace_id"`
	FolderID         uuid.UUID  `json:"folder_id"`
	Name             string     `json:"name"`
	CurrentVersionID *uuid.UUID `json:"current_version_id,omitempty"`
	SizeBytes        int64      `json:"size_bytes"`
	MimeType         string     `json:"mime_type"`
	CreatedBy        uuid.UUID  `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

// FileVersion is an immutable pointer to a single blob in zk-object-fabric.
type FileVersion struct {
	ID            uuid.UUID `json:"id"`
	FileID        uuid.UUID `json:"file_id"`
	VersionNumber int       `json:"version_number"`
	ObjectKey     string    `json:"object_key"`
	SizeBytes     int64     `json:"size_bytes"`
	Checksum      string    `json:"checksum"`
	CreatedBy     uuid.UUID `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}
