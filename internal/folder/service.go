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

// ErrInvalidEncryptionMode is returned when the caller supplies an
// encryption mode that is not in IsValidEncryptionMode.
var ErrInvalidEncryptionMode = errors.New("invalid encryption mode")

// ErrEncryptionModeMismatch is returned when a move would relocate a
// resource across folders with different encryption modes. Cross-mode
// moves require re-upload, not a metadata move, because the underlying
// objects live under different keying / placement regimes.
var ErrEncryptionModeMismatch = errors.New("encryption mode mismatch")

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
// A child folder always inherits its parent's encryption mode; the
// caller-supplied mode (via CreateWithMode) is only honoured for root
// folders so the column stays consistent within a subtree.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name string, createdBy uuid.UUID) (*Folder, error) {
	return s.CreateWithMode(ctx, workspaceID, parentID, name, "", createdBy)
}

// CreateWithMode is the encryption-mode-aware variant of Create. An
// empty mode falls back to EncryptionManagedEncrypted (or the parent's
// mode for non-root folders). Invalid modes return
// ErrInvalidEncryptionMode without touching the repository.
func (s *Service) CreateWithMode(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name, encryptionMode string, createdBy uuid.UUID) (*Folder, error) {
	name = strings.TrimSpace(name)
	if !isValidFolderName(name) {
		return nil, ErrInvalidName
	}
	if encryptionMode != "" && !IsValidEncryptionMode(encryptionMode) {
		return nil, ErrInvalidEncryptionMode
	}
	var path string
	mode := encryptionMode
	if parentID == nil {
		path = "/" + name + "/"
		if mode == "" {
			mode = EncryptionManagedEncrypted
		}
	} else {
		parent, err := s.repo.GetByID(ctx, workspaceID, *parentID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrInvalidParent
			}
			return nil, err
		}
		path = parent.Path + name + "/"
		// Sub-folders always inherit. Refusing to override here keeps
		// the encryption_mode column consistent within a subtree so
		// the worker / move-validation paths can trust the parent's
		// mode without walking the chain.
		mode = parent.EncryptionMode
		if mode == "" {
			mode = EncryptionManagedEncrypted
		}
	}
	f := &Folder{
		WorkspaceID:    workspaceID,
		ParentFolderID: parentID,
		Name:           name,
		Path:           path,
		EncryptionMode: mode,
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
		// Root folders inherit ManagedEncrypted by default; refuse to
		// move a strict-zk subtree to the root because a top-level
		// strict-zk folder is created explicitly via CreateWithMode.
		// Allowing a silent regrade here would let a member sidestep
		// the admin-controlled creation flow.
		if f.EncryptionMode != "" && f.EncryptionMode != EncryptionManagedEncrypted {
			return nil, ErrEncryptionModeMismatch
		}
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
		// Cross-mode moves require re-upload, not a metadata move,
		// because objects under different modes have different keying
		// / placement regimes.
		if !sameEncryptionMode(f.EncryptionMode, parent.EncryptionMode) {
			return nil, ErrEncryptionModeMismatch
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

// sameEncryptionMode treats an empty string as ManagedEncrypted so
// rows from before migration 018 (which is impossible after the
// column was added with a NOT NULL default, but kept defensive) are
// still considered managed-encrypted for compatibility.
func sameEncryptionMode(a, b string) bool {
	if a == "" {
		a = EncryptionManagedEncrypted
	}
	if b == "" {
		b = EncryptionManagedEncrypted
	}
	return a == b
}
