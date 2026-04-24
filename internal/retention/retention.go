// Package retention implements workspace retention policies: per-folder
// or workspace-wide rules that govern how many versions a file keeps,
// how long versions live before they are archived, and when they are
// hard-deleted. Policies are admin-managed; the retention worker reads
// them from this package and drives both the cold-archive and
// hard-delete passes.
package retention

import (
	"time"

	"github.com/google/uuid"
)

// Policy is a single retention_policies row. A nil FolderID means the
// policy applies workspace-wide; otherwise it is scoped to that
// folder (and, recursively, to its subtree — enforced in the
// evaluator, not the schema). Int pointers are used so a policy can
// leave a dimension unset without defaulting to zero.
type Policy struct {
	ID               uuid.UUID  `json:"id"`
	WorkspaceID      uuid.UUID  `json:"workspace_id"`
	FolderID         *uuid.UUID `json:"folder_id,omitempty"`
	MaxVersions      *int       `json:"max_versions,omitempty"`
	MaxAgeDays       *int       `json:"max_age_days,omitempty"`
	ArchiveAfterDays *int       `json:"archive_after_days,omitempty"`
	CreatedBy        *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// EvaluationResult is returned by Evaluate. It names the versions
// eligible for archival or deletion under the currently configured
// policies. Either slice can be empty; both can be empty if no policy
// is configured.
type EvaluationResult struct {
	WorkspaceID      uuid.UUID   `json:"workspace_id"`
	ArchiveVersions  []uuid.UUID `json:"archive_versions"`
	DeleteVersions   []uuid.UUID `json:"delete_versions"`
}
