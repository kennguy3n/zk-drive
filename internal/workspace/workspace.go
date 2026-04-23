package workspace

import (
	"time"

	"github.com/google/uuid"
)

// Tier describes the subscription tier of a workspace.
const (
	TierFree = "free"
	TierPro  = "pro"
)

// DefaultQuotaBytes is the free-tier storage quota (5 GB).
const DefaultQuotaBytes int64 = 5 * 1024 * 1024 * 1024

// Workspace is the tenant unit — every other resource belongs to a single
// workspace.
type Workspace struct {
	ID                uuid.UUID  `json:"id"`
	Name              string     `json:"name"`
	OwnerUserID       *uuid.UUID `json:"owner_user_id,omitempty"`
	StorageQuotaBytes int64      `json:"storage_quota_bytes"`
	StorageUsedBytes  int64      `json:"storage_used_bytes"`
	Tier              string     `json:"tier"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}
