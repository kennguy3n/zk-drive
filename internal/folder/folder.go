package folder

import (
	"time"

	"github.com/google/uuid"
)

// Encryption modes recognised by the folder layer. ManagedEncrypted is
// the default and matches the prior behaviour: server-side preview /
// scan / search are enabled and the gateway manages keys. StrictZK
// disables every server-side processing path; the file content is
// opaque to the server.
const (
	EncryptionManagedEncrypted = "managed_encrypted"
	EncryptionStrictZK         = "strict_zk"
)

// IsValidEncryptionMode reports whether m is one of the recognised
// encryption modes. Empty input is treated as invalid because the
// service layer always supplies a default before persisting.
func IsValidEncryptionMode(m string) bool {
	switch m {
	case EncryptionManagedEncrypted, EncryptionStrictZK:
		return true
	}
	return false
}

// Folder is a node in a workspace's folder tree. A nil ParentFolderID denotes
// a root-level folder.
type Folder struct {
	ID             uuid.UUID  `json:"id"`
	WorkspaceID    uuid.UUID  `json:"workspace_id"`
	ParentFolderID *uuid.UUID `json:"parent_folder_id,omitempty"`
	Name           string     `json:"name"`
	Path           string     `json:"path"`
	EncryptionMode string     `json:"encryption_mode"`
	CreatedBy      uuid.UUID  `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}
