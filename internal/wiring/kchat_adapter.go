package wiring

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

// KChatFolderAdapter bridges *folder.Service to kchat.FolderCreator.
type KChatFolderAdapter struct {
	Service *folder.Service
}

// NewKChatFolderCreator returns an adapter wrapping svc.
func NewKChatFolderCreator(svc *folder.Service) KChatFolderAdapter {
	return KChatFolderAdapter{Service: svc}
}

// Create proxies to folder.Service.Create.
func (a KChatFolderAdapter) Create(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name string, createdBy uuid.UUID) (kchat.FolderRef, error) {
	f, err := a.Service.Create(ctx, workspaceID, parentID, name, createdBy)
	if err != nil {
		return kchat.FolderRef{}, err
	}
	return kchat.FolderRef{ID: f.ID}, nil
}

// KChatPermissionAdapter bridges *permission.Service to
// kchat.PermissionGranter. SyncMembers needs ListForResource as well
// as Grant / Revoke, hence the dedicated adapter (the existing
// sharing adapter doesn't expose listing).
type KChatPermissionAdapter struct {
	Service *permission.Service
}

// NewKChatPermissionGranter returns an adapter wrapping svc.
func NewKChatPermissionGranter(svc *permission.Service) KChatPermissionAdapter {
	return KChatPermissionAdapter{Service: svc}
}

// Grant proxies to permission.Service.Grant.
func (a KChatPermissionAdapter) Grant(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, role string, expiresAt *time.Time) (kchat.PermissionRef, error) {
	p, err := a.Service.Grant(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, role, expiresAt)
	if err != nil {
		return kchat.PermissionRef{}, err
	}
	return kchat.PermissionRef{ID: p.ID}, nil
}

// Revoke proxies to permission.Service.Revoke.
func (a KChatPermissionAdapter) Revoke(ctx context.Context, workspaceID, permID uuid.UUID) error {
	return a.Service.Revoke(ctx, workspaceID, permID)
}

// ListForResource proxies to permission.Service.ListForResource and
// projects each row onto kchat.PermissionView so the kchat package
// stays free of the permission package import.
func (a KChatPermissionAdapter) ListForResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]kchat.PermissionView, error) {
	rows, err := a.Service.ListForResource(ctx, workspaceID, resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	out := make([]kchat.PermissionView, 0, len(rows))
	for _, p := range rows {
		out = append(out, kchat.PermissionView{
			ID:          p.ID,
			GranteeType: p.GranteeType,
			GranteeID:   p.GranteeID,
			Role:        p.Role,
		})
	}
	return out, nil
}

// KChatFileAdapter bridges *file.Service to kchat.FileCreator.
type KChatFileAdapter struct {
	Service *file.Service
}

// NewKChatFileCreator returns an adapter wrapping svc.
func NewKChatFileCreator(svc *file.Service) KChatFileAdapter {
	return KChatFileAdapter{Service: svc}
}

// Create proxies to file.Service.Create.
func (a KChatFileAdapter) Create(ctx context.Context, workspaceID, folderID uuid.UUID, name, mimeType string, createdBy uuid.UUID) (kchat.FileRef, error) {
	f, err := a.Service.Create(ctx, workspaceID, folderID, name, mimeType, createdBy)
	if err != nil {
		return kchat.FileRef{}, err
	}
	return kchat.FileRef{ID: f.ID, FolderID: f.FolderID}, nil
}

// GetByID proxies to file.Service.GetByID.
func (a KChatFileAdapter) GetByID(ctx context.Context, workspaceID, fileID uuid.UUID) (kchat.FileRef, error) {
	f, err := a.Service.GetByID(ctx, workspaceID, fileID)
	if err != nil {
		return kchat.FileRef{}, err
	}
	return kchat.FileRef{ID: f.ID, FolderID: f.FolderID}, nil
}

// ConfirmVersion translates the high-level confirm call back into a
// *file.FileVersion and forwards to file.Service.ConfirmVersion.
func (a KChatFileAdapter) ConfirmVersion(ctx context.Context, workspaceID, fileID uuid.UUID, objectKey, checksum string, sizeBytes int64, createdBy uuid.UUID) (kchat.FileVersionRef, error) {
	v := &file.FileVersion{
		FileID:    fileID,
		ObjectKey: objectKey,
		SizeBytes: sizeBytes,
		Checksum:  checksum,
		CreatedBy: createdBy,
	}
	if err := a.Service.ConfirmVersion(ctx, workspaceID, v); err != nil {
		return kchat.FileVersionRef{}, err
	}
	return kchat.FileVersionRef{ID: v.ID, ObjectKey: v.ObjectKey}, nil
}

// KChatPresignAdapter bridges *storage.ClientFactory to
// kchat.PresignResolver, returning a thin shim around *storage.Client
// so the kchat package never needs to import the storage package
// directly.
type KChatPresignAdapter struct {
	Factory *storage.ClientFactory
}

// NewKChatPresignResolver returns an adapter wrapping factory.
func NewKChatPresignResolver(factory *storage.ClientFactory) KChatPresignAdapter {
	return KChatPresignAdapter{Factory: factory}
}

// ForWorkspace returns the per-workspace storage client. The returned
// value is non-nil on success.
func (a KChatPresignAdapter) ForWorkspace(ctx context.Context, workspaceID uuid.UUID) (kchat.PresignClient, error) {
	c, err := a.Factory.ForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	return kchatPresignClient{c}, nil
}

type kchatPresignClient struct{ *storage.Client }

func (c kchatPresignClient) GenerateUploadURL(ctx context.Context, objectKey, contentType string, expiry time.Duration) (string, error) {
	return c.Client.GenerateUploadURL(ctx, objectKey, contentType, expiry)
}

// KChatObjectKey is the storage.NewObjectKey wrapper, exposed here
// so cmd/server / tests can pass a function value of the right type
// to kchat.NewRoomService.
func KChatObjectKey(workspaceID, fileID, versionID uuid.UUID) string {
	return storage.NewObjectKey(workspaceID, fileID, versionID)
}
