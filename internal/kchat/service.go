package kchat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FolderCreator is the tiny subset of folder.Service that the room
// service needs. Taking an interface keeps the kchat package free of
// an import cycle and makes the service unit-testable.
type FolderCreator interface {
	Create(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID, name string, createdBy uuid.UUID) (FolderRef, error)
}

// FolderRef is the minimum a FolderCreator must return — the ID of
// the created folder, which the mapping row stores as folder_id.
type FolderRef struct {
	ID uuid.UUID
}

// PermissionGranter is the subset of permission.Service the room
// service needs. The interface intentionally mirrors the shape used
// by sharing.PermissionGranter so wiring code can adapt the same
// concrete service to both packages.
type PermissionGranter interface {
	Grant(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, role string, expiresAt *time.Time) (PermissionRef, error)
	Revoke(ctx context.Context, workspaceID, permID uuid.UUID) error
	ListForResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]PermissionView, error)
}

// PermissionRef is the minimum a PermissionGranter.Grant must
// return — the ID of the new grant.
type PermissionRef struct {
	ID uuid.UUID
}

// PermissionView is the read shape used by ListForResource. Only the
// fields the sync algorithm reads are exposed so adapters can map
// from heterogeneous permission models without leaking internals.
type PermissionView struct {
	ID          uuid.UUID
	GranteeType string
	GranteeID   uuid.UUID
	Role        string
}

// FileCreator is the subset of file.Service the room service needs
// for the attachment flow. The room service creates the file
// metadata row up front so the client can reference a stable file
// ID when it later confirms the upload.
type FileCreator interface {
	Create(ctx context.Context, workspaceID, folderID uuid.UUID, name, mimeType string, createdBy uuid.UUID) (FileRef, error)
	GetByID(ctx context.Context, workspaceID, fileID uuid.UUID) (FileRef, error)
	ConfirmVersion(ctx context.Context, workspaceID, fileID uuid.UUID, objectKey, checksum string, sizeBytes int64, createdBy uuid.UUID) (FileVersionRef, error)
}

// FileRef is the minimum a FileCreator returns: an ID + folder so the
// service can validate that confirms target the right folder.
type FileRef struct {
	ID       uuid.UUID
	FolderID uuid.UUID
}

// FileVersionRef captures the new version's ID after a successful
// confirm so the caller can hand it back to the client.
type FileVersionRef struct {
	ID        uuid.UUID
	ObjectKey string
}

// PresignClient is the subset of storage.Client the service needs
// to mint upload URLs. The signature mirrors storage.Client.GenerateUploadURL
// so production adapters are essentially passthroughs.
type PresignClient interface {
	GenerateUploadURL(ctx context.Context, objectKey, contentType string, expiry time.Duration) (string, error)
}

// PresignResolver returns the per-workspace storage client. Tests can
// pass a nil resolver to disable upload-URL generation; the service
// then responds with an explicit error rather than panicking.
type PresignResolver interface {
	ForWorkspace(ctx context.Context, workspaceID uuid.UUID) (PresignClient, error)
}

// ObjectKeyFactory builds the storage object key for a new file
// version. Pulling this out of the service keeps the KChat
// integration aligned with the rest of zk-drive (which uses
// storage.NewObjectKey).
type ObjectKeyFactory func(workspaceID, fileID, versionID uuid.UUID) string

// RoomService owns the KChat room mapping lifecycle plus the helpers
// that drive permission sync and attachment uploads. All inputs are
// validated up front; all writes are workspace-scoped.
type RoomService struct {
	repo        Repository
	folders     FolderCreator
	permissions PermissionGranter
	files       FileCreator
	presign     PresignResolver
	keyFactory  ObjectKeyFactory
	now         func() time.Time

	// uploadExpiry controls the validity window for presigned PUT URLs
	// minted by AttachmentUploadURL. Defaults to 15 minutes (matching
	// storage.DefaultPresignExpiry) when zero.
	uploadExpiry time.Duration
}

// NewRoomService builds a RoomService. folders and permissions are
// required for the mapping + permission-sync paths; files / presign /
// keyFactory are only required by the attachment flow and may be nil
// in tests that exercise mapping-only behaviour.
func NewRoomService(repo Repository, folders FolderCreator, permissions PermissionGranter, files FileCreator, presign PresignResolver, keyFactory ObjectKeyFactory) *RoomService {
	return &RoomService{
		repo:        repo,
		folders:     folders,
		permissions: permissions,
		files:       files,
		presign:     presign,
		keyFactory:  keyFactory,
		now:         time.Now,
	}
}

// SetClock overrides the wall-clock source. Intended for tests.
func (s *RoomService) SetClock(now func() time.Time) { s.now = now }

// SetUploadExpiry overrides the presigned-URL validity window.
func (s *RoomService) SetUploadExpiry(d time.Duration) { s.uploadExpiry = d }

// CreateRoomFolder provisions a new folder named after the room,
// persists the (workspace, room) → folder mapping, and grants the
// caller admin permission on the folder. Returns ErrRoomAlreadyMapped
// if a mapping already exists for the same kchat_room_id.
//
// Callers should already have validated that the user is authorized
// to create rooms in the workspace; the service does not enforce
// admin-only here so tests can exercise the mapping logic with
// regular users.
func (s *RoomService) CreateRoomFolder(ctx context.Context, workspaceID uuid.UUID, kchatRoomID string, createdBy uuid.UUID) (*RoomFolder, error) {
	roomID := strings.TrimSpace(kchatRoomID)
	if roomID == "" {
		return nil, ErrInvalidRoomID
	}
	// Pre-check so we 409 without minting an orphan folder. There's
	// still a TOCTOU window where a concurrent request can win the
	// race between this lookup and the INSERT — Repository.Create
	// catches that via the unique-constraint violation translation
	// and surfaces ErrRoomAlreadyMapped.
	if existing, err := s.repo.GetByRoomID(ctx, workspaceID, roomID); err == nil && existing != nil {
		return nil, ErrRoomAlreadyMapped
	} else if err != nil && !errors.Is(err, ErrRoomNotFound) {
		return nil, fmt.Errorf("lookup existing mapping: %w", err)
	}

	fref, err := s.folders.Create(ctx, workspaceID, nil, folderNameFor(roomID), createdBy)
	if err != nil {
		return nil, fmt.Errorf("create room folder: %w", err)
	}

	row := &RoomFolder{
		WorkspaceID: workspaceID,
		KChatRoomID: roomID,
		FolderID:    fref.ID,
		CreatedBy:   createdBy,
	}
	if err := s.repo.Create(ctx, row); err != nil {
		// No compensating folder delete: folder.Service lacks a
		// handle here without widening the FolderCreator interface.
		// An orphan empty folder is a safer failure than half-state
		// in the mapping table; the retention worker can sweep.
		return nil, err
	}

	// Grant the creator admin permission on the folder. Failing this
	// step does not roll back the mapping: the caller can re-grant
	// via the standard /api/permissions endpoint, and rolling back
	// would require introducing a folder-delete on FolderCreator.
	if s.permissions != nil {
		if _, err := s.permissions.Grant(ctx, workspaceID, ResourceFolder, fref.ID, GranteeUser, createdBy, RoleAdmin, nil); err != nil {
			return row, fmt.Errorf("grant creator admin: %w", err)
		}
	}
	return row, nil
}

// Get fetches a mapping by id, scoped to workspace.
func (s *RoomService) Get(ctx context.Context, workspaceID, id uuid.UUID) (*RoomFolder, error) {
	return s.repo.Get(ctx, workspaceID, id)
}

// GetByRoomID fetches a mapping by the KChat-side room identifier.
func (s *RoomService) GetByRoomID(ctx context.Context, workspaceID uuid.UUID, kchatRoomID string) (*RoomFolder, error) {
	return s.repo.GetByRoomID(ctx, workspaceID, kchatRoomID)
}

// List returns every mapping in a workspace, newest first.
func (s *RoomService) List(ctx context.Context, workspaceID uuid.UUID) ([]*RoomFolder, error) {
	return s.repo.List(ctx, workspaceID)
}

// Delete removes the mapping row. The backing folder is intentionally
// left alone — operators may want to keep the uploaded files; the
// folder can be deleted through the regular folder API if desired.
func (s *RoomService) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	return s.repo.Delete(ctx, workspaceID, id)
}

// SyncMembers reconciles the supplied member set against the
// folder's current grants. Adds grants for new members, revokes
// grants for removed members, and updates role for members whose
// role changed. The set of members is treated as authoritative —
// any existing user grant on the folder that is not in members will
// be revoked. Guest grants (e.g. share-link callers) are left
// untouched so the KChat sync never clobbers public-link access.
func (s *RoomService) SyncMembers(ctx context.Context, workspaceID uuid.UUID, kchatRoomID string, members []MemberSync) error {
	if s.permissions == nil {
		return errors.New("kchat: permissions not configured")
	}
	roomID := strings.TrimSpace(kchatRoomID)
	if roomID == "" {
		return ErrInvalidRoomID
	}
	for _, m := range members {
		if !isValidRole(m.Role) {
			return fmt.Errorf("%w: %q", ErrInvalidRole, m.Role)
		}
	}

	mapping, err := s.repo.GetByRoomID(ctx, workspaceID, roomID)
	if err != nil {
		return err
	}

	current, err := s.permissions.ListForResource(ctx, workspaceID, ResourceFolder, mapping.FolderID)
	if err != nil {
		return fmt.Errorf("list current grants: %w", err)
	}

	// Index existing user grants by grantee_id. Multiple grants for
	// the same user on the same resource are theoretically possible;
	// we de-dupe here (keeping the highest-rank grant) so a noisy
	// history doesn't surface as multiple sync revocations on a
	// single sync. Guest grants are skipped entirely.
	existing := map[uuid.UUID][]PermissionView{}
	for _, p := range current {
		if p.GranteeType != GranteeUser {
			continue
		}
		existing[p.GranteeID] = append(existing[p.GranteeID], p)
	}

	desired := map[uuid.UUID]string{}
	for _, m := range members {
		desired[m.UserID] = m.Role
	}

	// Revoke first so role downgrades and removals run before the
	// adds. This prevents a "double-grant" intermediate state from
	// being visible to a concurrent reader.
	type revokeOp struct {
		userID  uuid.UUID
		permIDs []uuid.UUID
	}
	revokes := []revokeOp{}
	addOrChange := []MemberSync{}

	for userID, perms := range existing {
		role, want := desired[userID]
		if !want {
			ids := make([]uuid.UUID, 0, len(perms))
			for _, p := range perms {
				ids = append(ids, p.ID)
			}
			revokes = append(revokes, revokeOp{userID: userID, permIDs: ids})
			continue
		}
		// User is desired — keep the grant if any current grant
		// already matches the role; otherwise revoke and re-grant
		// so the role updates atomically.
		match := false
		var stale []uuid.UUID
		for _, p := range perms {
			if p.Role == role && !match {
				match = true
				continue
			}
			stale = append(stale, p.ID)
		}
		if len(stale) > 0 {
			revokes = append(revokes, revokeOp{userID: userID, permIDs: stale})
		}
		if !match {
			addOrChange = append(addOrChange, MemberSync{UserID: userID, Role: role})
		}
	}
	for userID, role := range desired {
		if _, ok := existing[userID]; ok {
			continue
		}
		addOrChange = append(addOrChange, MemberSync{UserID: userID, Role: role})
	}

	// Sort revokes for deterministic test assertions.
	sort.Slice(revokes, func(i, j int) bool {
		return revokes[i].userID.String() < revokes[j].userID.String()
	})
	sort.Slice(addOrChange, func(i, j int) bool {
		return addOrChange[i].UserID.String() < addOrChange[j].UserID.String()
	})

	for _, r := range revokes {
		for _, pid := range r.permIDs {
			if err := s.permissions.Revoke(ctx, workspaceID, pid); err != nil {
				return fmt.Errorf("revoke grant %s: %w", pid, err)
			}
		}
	}
	for _, m := range addOrChange {
		if _, err := s.permissions.Grant(ctx, workspaceID, ResourceFolder, mapping.FolderID, GranteeUser, m.UserID, m.Role, nil); err != nil {
			return fmt.Errorf("grant %s/%s: %w", m.UserID, m.Role, err)
		}
	}
	return nil
}

// AttachmentUploadResult is the response shape returned by
// AttachmentUploadURL. ObjectKey is opaque to clients but must be
// echoed back verbatim on confirm so the server records the exact
// key it signed.
type AttachmentUploadResult struct {
	UploadURL string    `json:"upload_url"`
	UploadID  uuid.UUID `json:"upload_id"`
	ObjectKey string    `json:"object_key"`
	FolderID  uuid.UUID `json:"folder_id"`
}

// AttachmentUploadURL resolves the room's folder, creates the file
// metadata row, generates a presigned PUT URL, and returns the URL +
// stable file ID. Mirrors api/drive/handler.go's UploadURL flow but
// scoped to the room's folder so KChat clients don't need to know
// the underlying ZK Drive folder ID.
func (s *RoomService) AttachmentUploadURL(ctx context.Context, workspaceID uuid.UUID, kchatRoomID, filename, contentType string, sizeBytes int64, createdBy uuid.UUID) (*AttachmentUploadResult, error) {
	if s.files == nil || s.presign == nil || s.keyFactory == nil {
		return nil, errors.New("kchat: attachment flow not configured")
	}
	if sizeBytes < 0 {
		return nil, ErrInvalidSize
	}
	mapping, err := s.repo.GetByRoomID(ctx, workspaceID, kchatRoomID)
	if err != nil {
		return nil, err
	}
	store, err := s.presign.ForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve storage: %w", err)
	}
	if store == nil {
		return nil, errors.New("kchat: storage not configured for workspace")
	}
	f, err := s.files.Create(ctx, workspaceID, mapping.FolderID, filename, contentType, createdBy)
	if err != nil {
		return nil, fmt.Errorf("create file metadata: %w", err)
	}
	versionID := uuid.New()
	objectKey := s.keyFactory(workspaceID, f.ID, versionID)

	expiry := s.uploadExpiry
	if expiry == 0 {
		expiry = 15 * time.Minute
	}
	url, err := store.GenerateUploadURL(ctx, objectKey, contentType, expiry)
	if err != nil {
		return nil, fmt.Errorf("generate upload url: %w", err)
	}
	return &AttachmentUploadResult{
		UploadURL: url,
		UploadID:  f.ID,
		ObjectKey: objectKey,
		FolderID:  mapping.FolderID,
	}, nil
}

// AttachmentConfirmResult is returned by ConfirmAttachment so callers
// can echo the new version ID back to the KChat client.
type AttachmentConfirmResult struct {
	FileID    uuid.UUID `json:"file_id"`
	VersionID uuid.UUID `json:"version_id"`
}

// ConfirmAttachment promotes a previously-minted upload URL into a
// FileVersion. The object_key is bound to the workspace + file; the
// service rejects mismatches so a malicious caller cannot confirm
// against a key that didn't come from AttachmentUploadURL.
func (s *RoomService) ConfirmAttachment(ctx context.Context, workspaceID, fileID uuid.UUID, objectKey, checksum string, sizeBytes int64, createdBy uuid.UUID) (*AttachmentConfirmResult, error) {
	if s.files == nil {
		return nil, errors.New("kchat: attachment flow not configured")
	}
	if strings.TrimSpace(objectKey) == "" {
		return nil, ErrInvalidObjectKey
	}
	if sizeBytes < 0 {
		return nil, ErrInvalidSize
	}
	f, err := s.files.GetByID(ctx, workspaceID, fileID)
	if err != nil {
		return nil, err
	}
	expectedPrefix := workspaceID.String() + "/" + f.ID.String() + "/"
	if !strings.HasPrefix(objectKey, expectedPrefix) {
		return nil, fmt.Errorf("%w %s", ErrObjectKeyMismatch, fileID)
	}
	v, err := s.files.ConfirmVersion(ctx, workspaceID, f.ID, objectKey, checksum, sizeBytes, createdBy)
	if err != nil {
		return nil, fmt.Errorf("confirm version: %w", err)
	}
	return &AttachmentConfirmResult{FileID: f.ID, VersionID: v.ID}, nil
}

// Permission constants re-exposed locally so callers don't have to
// import the permission package just to map roles. The values are
// the canonical strings used in the permissions table.
const (
	ResourceFolder = "folder"
	GranteeUser    = "user"
	RoleAdmin      = "admin"
	RoleEditor     = "editor"
	RoleViewer     = "viewer"
)

func isValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleEditor, RoleViewer:
		return true
	}
	return false
}

// folderNameFor produces a folder name for a KChat room. Slashes are
// replaced with dashes so the materialized folder path stays well-
// formed even when the upstream room id contains them.
func folderNameFor(kchatRoomID string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(kchatRoomID), "/", "-")
	if cleaned == "" {
		return "kchat-room"
	}
	return "KChat: " + cleaned
}
