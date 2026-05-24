package file

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidName is returned when a file name is empty.
var ErrInvalidName = errors.New("invalid file name")

// ErrInvalidTag is returned when a tag is empty after trimming or
// exceeds the soft cap. Distinct from ErrInvalidName so handlers can
// surface a tag-specific error message.
var ErrInvalidTag = errors.New("invalid tag")

// MaxTagLength caps individual tag strings. Tags are intended for
// short labels (project names, status flags), so 64 chars is plenty
// and keeps the (workspace, tag) index narrow.
const MaxTagLength = 64

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

// ListInFolderSubtree returns every non-deleted file under folderID
// (including those in nested descendant folders). Callers use this
// to snapshot per-file metadata BEFORE issuing a recursive folder
// soft-delete so they can emit a file.deleted webhook per affected
// file (the folder cascade soft-deletes the files in the same
// transaction, and after that point the snapshot rows are no longer
// visible to GetByID's deleted_at IS NULL filter).
func (s *Service) ListInFolderSubtree(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error) {
	return s.repo.ListFilesInFolderSubtree(ctx, workspaceID, folderID)
}

// SetPendingUploadObjectKey records the presigned-PUT object_key on
// a file row so the orphan-object GC reconciler can reclaim the S3
// object if ConfirmUpload never completes. Called from the UploadURL
// handler immediately after Create + key derivation.
func (s *Service) SetPendingUploadObjectKey(ctx context.Context, workspaceID, fileID uuid.UUID, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("pending upload object key required")
	}
	return s.repo.SetPendingUploadObjectKey(ctx, workspaceID, fileID, key)
}

// ListPendingUploadOrphans returns file rows whose presigned upload was
// minted before olderThan and which still have no confirmed
// current_version_id. The orphan-object GC reconciler calls this
// per-workspace, then issues DeleteObject + DeletePendingOrphan for
// each entry.
func (s *Service) ListPendingUploadOrphans(ctx context.Context, workspaceID uuid.UUID, olderThan time.Time, limit int) ([]*PendingOrphan, error) {
	return s.repo.ListPendingUploadOrphans(ctx, workspaceID, olderThan, limit)
}

// DeletePendingOrphan removes an orphan file row after the storage
// object has been (best-effort) deleted. The predicate-guarded SQL
// in the repository ensures a confirm that landed between the list
// and the delete cannot have its row deleted out from under it.
func (s *Service) DeletePendingOrphan(ctx context.Context, workspaceID, fileID uuid.UUID) error {
	return s.repo.DeletePendingOrphan(ctx, workspaceID, fileID)
}

// CreateVersion inserts a file version row and updates the file's current
// version pointer atomically. Intended for use by the upload-confirmation
// endpoint once the S3 integration lands in Phase 1b.
func (s *Service) CreateVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error {
	return s.repo.CreateVersionAndSetCurrent(ctx, workspaceID, v)
}

// ConfirmVersion inserts a file version, advances current_version_id,
// and updates the file's size_bytes atomically. The `fresh` return
// flag is true when the call actually created a new row and false
// when it hit the idempotent-replay path (a row with the same v.ID
// already existed). Callers that need to gate one-shot side effects
// (activity logs, billing usage events, post-upload jobs) MUST
// inspect `fresh`; see the repository-level doc comment for the
// full semantics.
func (s *Service) ConfirmVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) (bool, error) {
	if v.SizeBytes < 0 {
		return false, errors.New("size_bytes must be non-negative")
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

// GetVersionByID returns a single version scoped to a workspace. Handlers
// that hold a file's current_version_id use this instead of ListVersions
// so they do not pay for a full history fetch.
func (s *Service) GetVersionByID(ctx context.Context, workspaceID, versionID uuid.UUID) (*FileVersion, error) {
	return s.repo.GetVersionByID(ctx, workspaceID, versionID)
}

// AddTag attaches a tag to a file. The tag is trimmed and lowercased
// so callers don't have to normalise on the way in; case-insensitive
// equality is the common expectation for labels.
func (s *Service) AddTag(ctx context.Context, workspaceID, fileID, createdBy uuid.UUID, tag string) (*Tag, error) {
	tag = normalizeTag(tag)
	if tag == "" || len(tag) > MaxTagLength {
		return nil, ErrInvalidTag
	}
	return s.repo.AddTag(ctx, workspaceID, fileID, createdBy, tag)
}

// RemoveTag detaches a single (file, tag) pair.
func (s *Service) RemoveTag(ctx context.Context, workspaceID, fileID uuid.UUID, tag string) error {
	tag = normalizeTag(tag)
	if tag == "" {
		return ErrInvalidTag
	}
	return s.repo.RemoveTag(ctx, workspaceID, fileID, tag)
}

// ListTags returns every tag attached to a file.
func (s *Service) ListTags(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*Tag, error) {
	return s.repo.ListTagsByFile(ctx, workspaceID, fileID)
}

// ListWorkspaceTags returns every tag in a workspace.
func (s *Service) ListWorkspaceTags(ctx context.Context, workspaceID uuid.UUID) ([]*Tag, error) {
	return s.repo.ListTagsByWorkspace(ctx, workspaceID)
}

// normalizeTag trims whitespace and lowercases the tag, then rejects
// any tag containing characters whose URL-encoding semantics make the
// path-param round-trip ambiguous: `/` (chi's path separator) and `%`
// (the URL-encoding sentinel — `net/http` already decodes
// `Request.URL.Path` so we can't safely double-decode in the handler).
// Returns "" for invalid tags so callers can map to ErrInvalidTag.
func normalizeTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if strings.ContainsAny(t, "/%") {
		return ""
	}
	return t
}
