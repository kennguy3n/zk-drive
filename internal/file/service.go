package file

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// ErrInvalidName is returned when a file name is empty.
var ErrInvalidName = errors.New("invalid file name")

// Service wraps the file repository with higher-level operations used by the
// HTTP handlers.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create inserts a file metadata row. Versioning is handled separately.
func (s *Service) Create(ctx context.Context, workspaceID, folderID uuid.UUID, name, mimeType string, createdBy uuid.UUID) (*File, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidName
	}
	f := &File{
		WorkspaceID: workspaceID,
		FolderID:    folderID,
		Name:        name,
		MimeType:    mimeType,
		CreatedBy:   createdBy,
	}
	if err := s.repo.CreateFile(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// GetByID returns a file.
func (s *Service) GetByID(ctx context.Context, workspaceID, fileID uuid.UUID) (*File, error) {
	return s.repo.GetFileByID(ctx, workspaceID, fileID)
}

// Rename updates the file's display name. Uses a name-only UPDATE so a
// concurrent Move cannot be silently reverted.
func (s *Service) Rename(ctx context.Context, workspaceID, fileID uuid.UUID, newName string) (*File, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, ErrInvalidName
	}
	if err := s.repo.RenameFile(ctx, workspaceID, fileID, newName); err != nil {
		return nil, err
	}
	return s.repo.GetFileByID(ctx, workspaceID, fileID)
}

// Move updates the file's parent folder.
func (s *Service) Move(ctx context.Context, workspaceID, fileID, folderID uuid.UUID) (*File, error) {
	if err := s.repo.MoveFile(ctx, workspaceID, fileID, folderID); err != nil {
		return nil, err
	}
	return s.repo.GetFileByID(ctx, workspaceID, fileID)
}

// Delete soft-deletes a file.
func (s *Service) Delete(ctx context.Context, workspaceID, fileID uuid.UUID) error {
	return s.repo.DeleteFile(ctx, workspaceID, fileID)
}

// ListByFolder returns files in a folder.
func (s *Service) ListByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error) {
	return s.repo.ListFilesByFolder(ctx, workspaceID, folderID)
}

// CreateVersion inserts a file version row and updates the file's current
// version pointer atomically. Intended for use by the upload-confirmation
// endpoint once the S3 integration lands in Phase 1b.
func (s *Service) CreateVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error {
	return s.repo.CreateVersionAndSetCurrent(ctx, workspaceID, v)
}

// ConfirmVersion inserts a file version, advances current_version_id, and
// updates the file's size_bytes atomically. Used by the upload-confirm
// endpoint so a retry after a partial failure cannot produce duplicate
// versions or a stale size_bytes.
func (s *Service) ConfirmVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error {
	if v.SizeBytes < 0 {
		return errors.New("size_bytes must be non-negative")
	}
	return s.repo.ConfirmVersion(ctx, workspaceID, v)
}

// UpdateSize records a new byte-size on the file row. Called by the
// upload-confirmation endpoint so listings reflect the latest version's
// size without a join.
func (s *Service) UpdateSize(ctx context.Context, workspaceID, fileID uuid.UUID, sizeBytes int64) error {
	if sizeBytes < 0 {
		return errors.New("size_bytes must be non-negative")
	}
	return s.repo.UpdateFileSize(ctx, workspaceID, fileID, sizeBytes)
}

// ListVersions returns all versions of a file.
func (s *Service) ListVersions(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*FileVersion, error) {
	return s.repo.ListVersions(ctx, workspaceID, fileID)
}
