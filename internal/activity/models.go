// Package activity implements a fire-and-forget audit log for workspace
// CRUD operations. Every successful folder / file / permission mutation
// calls Service.Log with a LogEntry; the service drains entries onto a
// background goroutine so an overloaded or temporarily unavailable
// activity_log table never stalls the parent HTTP request.
package activity

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Action constants name the events recorded in activity_log. These strings
// are deliberately dotted ("file.create") so the frontend can group by
// prefix without parsing two columns.
const (
	ActionFileCreate   = "file.create"
	ActionFileRename   = "file.rename"
	ActionFileMove     = "file.move"
	ActionFileDelete   = "file.delete"
	ActionFileUpload   = "file.upload"
	ActionFileDownload = "file.download"
	ActionFolderCreate = "folder.create"
	ActionFolderRename = "folder.rename"
	ActionFolderMove   = "folder.move"
	ActionFolderDelete = "folder.delete"
	ActionPermGrant    = "permission.grant"
	ActionPermRevoke   = "permission.revoke"
)

// LogEntry is a single row of the activity_log table. MetadataJSON is a
// free-form JSONB blob for action-specific context (e.g. old_name /
// new_name on rename); callers should keep it small.
type LogEntry struct {
	ID           uuid.UUID       `json:"id"`
	WorkspaceID  uuid.UUID       `json:"workspace_id"`
	UserID       uuid.UUID       `json:"user_id"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   uuid.UUID       `json:"resource_id"`
	MetadataJSON json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}
