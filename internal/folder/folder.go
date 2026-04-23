package folder

import (
	"time"

	"github.com/google/uuid"
)

// Folder is a node in a workspace's folder tree. A nil ParentFolderID denotes
// a root-level folder.
type Folder struct {
	ID             uuid.UUID  `json:"id"`
	WorkspaceID    uuid.UUID  `json:"workspace_id"`
	ParentFolderID *uuid.UUID `json:"parent_folder_id,omitempty"`
	Name           string     `json:"name"`
	Path           string     `json:"path"`
	CreatedBy      uuid.UUID  `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}
