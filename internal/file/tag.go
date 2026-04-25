package file

import (
	"time"

	"github.com/google/uuid"
)

// Tag is a user-attached label on a file. Tags are workspace-scoped so
// admins can search for "all files in workspace X tagged contract"
// without crossing tenant boundaries; the unique index on
// (file_id, tag) keeps duplicate inserts honest at the DB layer.
type Tag struct {
	ID          uuid.UUID `json:"id"`
	FileID      uuid.UUID `json:"file_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Tag         string    `json:"tag"`
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}
