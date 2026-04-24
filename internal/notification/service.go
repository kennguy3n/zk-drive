package notification

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Service wires notification creation primitives on top of the
// repository. Methods are typed per-event so callers do not have to
// know the internal type string constants.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// List returns notifications for a user, unread first. limit is
// clamped to [1, 100]; offset is floored at 0.
func (s *Service) List(ctx context.Context, workspaceID, userID uuid.UUID, limit, offset int) ([]*Notification, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.ListForUser(ctx, workspaceID, userID, limit, offset)
}

// MarkRead flips a single notification to read for the caller.
func (s *Service) MarkRead(ctx context.Context, workspaceID, userID, id uuid.UUID) error {
	return s.repo.MarkRead(ctx, workspaceID, userID, id)
}

// MarkAllRead flips every unread notification for the caller.
func (s *Service) MarkAllRead(ctx context.Context, workspaceID, userID uuid.UUID) error {
	return s.repo.MarkAllRead(ctx, workspaceID, userID)
}

// NotifyShareLinkCreated informs the resource owner that a new share
// link was minted for one of their resources.
func (s *Service) NotifyShareLinkCreated(ctx context.Context, workspaceID, ownerID, linkID uuid.UUID, resourceType string, resourceID uuid.UUID) error {
	return s.create(ctx, &Notification{
		WorkspaceID:  workspaceID,
		UserID:       ownerID,
		Type:         TypeShareLinkCreated,
		Title:        "Share link created",
		Body:         fmt.Sprintf("A new share link was created for a %s.", resourceType),
		ResourceType: stringPtr("share_link"),
		ResourceID:   &linkID,
	})
}

// NotifyGuestInviteSent informs an invitee (identified by user_id
// inside this workspace). Callers that can't resolve the invitee to a
// user (external email, new account) should skip this call.
func (s *Service) NotifyGuestInviteSent(ctx context.Context, workspaceID, inviteeUserID, inviteID, folderID uuid.UUID, email string) error {
	return s.create(ctx, &Notification{
		WorkspaceID:  workspaceID,
		UserID:       inviteeUserID,
		Type:         TypeGuestInviteSent,
		Title:        "You were invited to a folder",
		Body:         fmt.Sprintf("%s was invited as a guest.", email),
		ResourceType: stringPtr("guest_invite"),
		ResourceID:   &inviteID,
	})
}

// NotifyGuestInviteAccepted informs the invite creator that the
// invitee accepted.
func (s *Service) NotifyGuestInviteAccepted(ctx context.Context, workspaceID, creatorID, inviteID uuid.UUID, email string) error {
	return s.create(ctx, &Notification{
		WorkspaceID:  workspaceID,
		UserID:       creatorID,
		Type:         TypeGuestInviteAccepted,
		Title:        "Guest invite accepted",
		Body:         fmt.Sprintf("%s accepted your invitation.", email),
		ResourceType: stringPtr("guest_invite"),
		ResourceID:   &inviteID,
	})
}

// NotifyQuarantine fans out a quarantine notification to every admin
// of the workspace. Admins are resolved via the users table.
func (s *Service) NotifyQuarantine(ctx context.Context, workspaceID, fileID, versionID uuid.UUID, signature string) error {
	admins, err := s.repo.ListWorkspaceAdmins(ctx, workspaceID)
	if err != nil {
		return err
	}
	title := "File quarantined"
	body := fmt.Sprintf("ClamAV flagged a file version (%s).", signature)
	for _, adminID := range admins {
		n := &Notification{
			WorkspaceID:  workspaceID,
			UserID:       adminID,
			Type:         TypeScanQuarantined,
			Title:        title,
			Body:         body,
			ResourceType: stringPtr("file_version"),
			ResourceID:   &versionID,
		}
		if err := s.repo.Create(ctx, n); err != nil {
			return err
		}
	}
	_ = fileID // reserved for future per-file deep links
	return nil
}

func (s *Service) create(ctx context.Context, n *Notification) error {
	return s.repo.Create(ctx, n)
}

func stringPtr(s string) *string { return &s }
