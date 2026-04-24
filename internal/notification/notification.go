// Package notification implements in-app notifications for share
// links, guest invites, and scan quarantine events. Notifications are
// workspace- and user-scoped; the service layer fans out a single
// event to one or more users based on the notification type.
//
// Retention: notifications are kept indefinitely today and pruned
// manually via admin tooling. Phase 3 will add a retention policy.
package notification

import (
	"time"

	"github.com/google/uuid"
)

// Notification type constants. Kept as strings (not an enum) so the
// migration schema stays simple and the frontend can render custom
// renderers per type without database changes.
const (
	TypeShareLinkCreated     = "share_link.created"
	TypeGuestInviteSent      = "guest_invite.sent"
	TypeGuestInviteAccepted  = "guest_invite.accepted"
	TypeScanQuarantined      = "scan.quarantined"
)

// Notification mirrors the notifications table columns. ReadAt is nil
// until the recipient explicitly marks it read.
type Notification struct {
	ID           uuid.UUID  `json:"id"`
	WorkspaceID  uuid.UUID  `json:"workspace_id"`
	UserID       uuid.UUID  `json:"user_id"`
	Type         string     `json:"type"`
	Title        string     `json:"title"`
	Body         string     `json:"body"`
	ResourceType *string    `json:"resource_type,omitempty"`
	ResourceID   *uuid.UUID `json:"resource_id,omitempty"`
	ReadAt       *time.Time `json:"read_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}
