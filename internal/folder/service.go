package folder

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// ErrInvalidName is returned when a folder name is empty or otherwise
// unacceptable.
var ErrInvalidName = errors.New("invalid folder name")

// ErrInvalidParent is returned when a supplied parent folder id does not
// resolve to a folder in the same workspace, or when a move would create a
// cycle.
var ErrInvalidParent = errors.New("invalid parent folder")

// Service wraps the folder repository with path-computation and move
// validation.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create inserts a new folder. If parentID is non-nil, it must reference a
// folder in the same workspace. The path is computed relative to the parent.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name string, createdBy uuid.UUID) (*Folder, error) {
	name = strings.TrimSpace(name)
	if !isValidFolderName(name) {
		return nil, ErrInvalidName
	}
	var path string
	if parentID == nil {
		path = "/" + name + "/"
	} else {
		parent, err := s.repo.GetByID(ctx, workspaceID, *parentID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrInvalidParent
			}
			return nil, err
		}
		path = parent.Path + name + "/"
	}
	f := &Folder{
		WorkspaceID:    workspaceID,
		ParentFolderID: parentID,
		Name:           name,
		Path:           path,
		CreatedBy:      createdBy,
	}
	if err := s.repo.Create(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// GetByID fetches a folder.
func (s *Service) GetByID(ctx context.Context, workspaceID, folderID uuid.UUID) (*Folder, error) {
	return s.repo.GetByID(ctx, workspaceID, folderID)
}

// Rename updates the folder's name and path (plus its descendants' paths).
func (s *Service) Rename(ctx context.Context, workspaceID, folderID uuid.UUID, newName string) (*Folder, error) {
	newName = strings.TrimSpace(newName)
	if !isValidFolderName(newName) {
		return nil, ErrInvalidName
	}
	f, err := s.repo.GetByID(ctx, workspaceID, folderID)
	if err != nil {
		return nil, err
	}
	oldPath := f.Path
	parentPath := "/"
	if trimmed := strings.TrimSuffix(oldPath, "/"); trimmed != "" {
		if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
			parentPath = trimmed[:idx+1]
		}
	}
	newPath := parentPath + newName + "/"
	if err := s.repo.RenameWithDescendants(ctx, workspaceID, folderID, newName, oldPath, newPath); err != nil {
		return nil, err
	}
	f.Name = newName
	f.Path = newPath
	return f, nil
}

// Move relocates a folder under a new parent (or to the root when
// newParentID is nil) and rewrites descendant paths accordingly.
func (s *Service) Move(ctx context.Context, workspaceID, folderID uuid.UUID, newParentID *uuid.UUID) (*Folder, error) {
	f, err := s.repo.GetByID(ctx, workspaceID, folderID)
	if err != nil {
		return nil, err
	}

	var newParentPath string
	if newParentID == nil {
		newParentPath = "/"
	} else {
		if *newParentID == folderID {
			return nil, ErrInvalidParent
		}
		parent, err := s.repo.GetByID(ctx, workspaceID, *newParentID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrInvalidParent
			}
			return nil, err
		}
		// Prevent cycles: the new parent must not be a descendant of f.
		if strings.HasPrefix(parent.Path, f.Path) {
			return nil, ErrInvalidParent
		}
		newParentPath = parent.Path
	}

	oldPath := f.Path
	newPath := newParentPath + f.Name + "/"
	if err := s.repo.MoveWithDescendants(ctx, workspaceID, folderID, newParentID, oldPath, newPath); err != nil {
		return nil, err
	}
	f.ParentFolderID = newParentID
	f.Path = newPath
	return f, nil
}

// Delete soft-deletes a folder and everything under it.
func (s *Service) Delete(ctx context.Context, workspaceID, folderID uuid.UUID) error {
	return s.repo.SoftDeleteSubtree(ctx, workspaceID, folderID)
}

// ListChildren returns direct child folders of the given parent (or root).
func (s *Service) ListChildren(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID) ([]*Folder, error) {
	return s.repo.ListChildren(ctx, workspaceID, parentID)
}

// isValidFolderName rejects names that would corrupt the materialized path.
// '/' is the path separator; empty names are disallowed on write.
func isValidFolderName(name string) bool {
	return name != "" && !strings.Contains(name, "/")
}
