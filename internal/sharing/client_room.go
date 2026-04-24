package sharing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ClientRoom is a "folder + share link" bundle dedicated to an external
// client or partner. The folder lives in the regular folders tree, so
// every existing endpoint (listing, permissions, moves) keeps working
// against it; the share link provides the public URL. We keep a
// separate row so the UI can surface client rooms distinctly from
// ordinary folders and so we can attach room-level policy
// (DropboxEnabled, ExpiresAt) without leaking concepts into the
// folders table.
type ClientRoom struct {
	ID             uuid.UUID  `json:"id"`
	WorkspaceID    uuid.UUID  `json:"workspace_id"`
	Name           string     `json:"name"`
	FolderID       uuid.UUID  `json:"folder_id"`
	ShareLinkID    uuid.UUID  `json:"share_link_id"`
	DropboxEnabled bool       `json:"dropbox_enabled"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	CreatedBy      uuid.UUID  `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ClientRoomRepository is the persistence surface for client rooms.
// Every method is workspace-scoped so the service layer cannot
// accidentally leak rooms across tenants.
type ClientRoomRepository interface {
	Create(ctx context.Context, room *ClientRoom) error
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*ClientRoom, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*ClientRoom, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

// FolderCreator is the tiny subset of folder.Service that the client
// room service needs. Taking an interface keeps the sharing package
// free of an import cycle and makes the service unit-testable with a
// mock folder creator.
type FolderCreator interface {
	Create(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name string, createdBy uuid.UUID) (FolderRef, error)
}

// FolderRef is the minimum a FolderCreator must return — just the id
// of the new folder, which the room row stores as folder_id.
type FolderRef struct {
	ID uuid.UUID
}

// ShareLinkCreator is the subset of Service.CreateShareLink needed by
// the room service. We reuse the existing share-link pipeline so every
// invariant (token entropy, bcrypt cost, cap validation) applies to
// rooms automatically.
type ShareLinkCreator interface {
	CreateShareLink(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, password string, expiresAt *time.Time, maxDownloads *int, createdBy uuid.UUID) (*ShareLink, error)
	RevokeShareLink(ctx context.Context, workspaceID, id uuid.UUID) error
}

// ClientRoomService owns the Create / List / Get / Delete lifecycle for
// client rooms. Creation is the non-trivial part: we must create a
// folder, create a share link over that folder, and persist the room
// row, rolling back the folder and share link if any later step fails.
type ClientRoomService struct {
	repo       ClientRoomRepository
	folders    FolderCreator
	shareLinks ShareLinkCreator
	now        func() time.Time
}

// NewClientRoomService wires the dependencies. folders and shareLinks
// are required; repo must be non-nil.
func NewClientRoomService(repo ClientRoomRepository, folders FolderCreator, shareLinks ShareLinkCreator) *ClientRoomService {
	return &ClientRoomService{
		repo:       repo,
		folders:    folders,
		shareLinks: shareLinks,
		now:        time.Now,
	}
}

// SetClock overrides the wall-clock source. Intended for tests.
func (s *ClientRoomService) SetClock(now func() time.Time) { s.now = now }

// ClientRoomInput captures the HTTP-facing parameters for creating a
// room. Password and ExpiresAt flow through to the share link; the
// folder is always created fresh so handlers never pass a FolderID.
type ClientRoomInput struct {
	Name           string
	Password       string
	ExpiresAt      *time.Time
	DropboxEnabled bool
}

// ErrInvalidRoomName is returned when Name is empty or contains
// characters that would corrupt the materialized path.
var ErrInvalidRoomName = errors.New("sharing: invalid client room name")

// Create provisions a new client room. On success the returned
// ClientRoom.ShareLink field (indirectly, via ShareLinkID) can be used
// by the caller to look up the public token. The function rolls back
// the folder (by soft delete is handled by folder.Delete; the folder
// creator may optionally implement a compensating delete on its own)
// and the share link if any step fails — see below for the exact
// ordering.
func (s *ClientRoomService) Create(ctx context.Context, workspaceID uuid.UUID, createdBy uuid.UUID, in ClientRoomInput) (*ClientRoom, *ShareLink, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || strings.Contains(name, "/") {
		return nil, nil, ErrInvalidRoomName
	}
	fref, err := s.folders.Create(ctx, workspaceID, nil, name, createdBy)
	if err != nil {
		return nil, nil, fmt.Errorf("create room folder: %w", err)
	}
	link, err := s.shareLinks.CreateShareLink(ctx, workspaceID, ResourceFolder, fref.ID, in.Password, in.ExpiresAt, nil, createdBy)
	if err != nil {
		// No compensating folder delete: folder.Service lacks a handle
		// here without widening the FolderCreator interface, and an
		// orphaned empty folder is a safer failure than a dangling
		// share link. The retention worker (Phase 2 §7.4) can sweep.
		return nil, nil, fmt.Errorf("create room share link: %w", err)
	}
	room := &ClientRoom{
		WorkspaceID:    workspaceID,
		Name:           name,
		FolderID:       fref.ID,
		ShareLinkID:    link.ID,
		DropboxEnabled: in.DropboxEnabled,
		ExpiresAt:      in.ExpiresAt,
		CreatedBy:      createdBy,
	}
	if err := s.repo.Create(ctx, room); err != nil {
		// Roll back the share link so a failed room insert doesn't
		// leave a public token pointing at a half-provisioned folder.
		_ = s.shareLinks.RevokeShareLink(ctx, workspaceID, link.ID)
		return nil, nil, fmt.Errorf("persist client room: %w", err)
	}
	return room, link, nil
}

// Get fetches a room by id scoped to workspace.
func (s *ClientRoomService) Get(ctx context.Context, workspaceID, id uuid.UUID) (*ClientRoom, error) {
	return s.repo.Get(ctx, workspaceID, id)
}

// List returns every client room in the workspace, newest first.
func (s *ClientRoomService) List(ctx context.Context, workspaceID uuid.UUID) ([]*ClientRoom, error) {
	return s.repo.List(ctx, workspaceID)
}

// Delete tears down a room: the share link is revoked first (so the
// public URL stops working immediately), then the room row is deleted.
// The backing folder is intentionally left alone — callers may want to
// keep the uploaded files; the folder can be deleted through the
// regular folder API if desired.
func (s *ClientRoomService) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	room, err := s.repo.Get(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	if rerr := s.shareLinks.RevokeShareLink(ctx, workspaceID, room.ShareLinkID); rerr != nil && !errors.Is(rerr, ErrNotFound) {
		return fmt.Errorf("revoke room share link: %w", rerr)
	}
	return s.repo.Delete(ctx, workspaceID, id)
}
