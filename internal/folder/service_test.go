package folder

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeRepo is a minimal in-memory Repository for exercising the
// Service's root-folder default-mode resolution. Only the methods the
// create path touches are meaningful; the rest satisfy the interface.
type fakeRepo struct {
	created *Folder
	byID    map[uuid.UUID]*Folder
}

func (f *fakeRepo) Create(_ context.Context, fl *Folder) error {
	f.created = fl
	return nil
}
func (f *fakeRepo) GetByID(_ context.Context, _ uuid.UUID, folderID uuid.UUID) (*Folder, error) {
	if fl, ok := f.byID[folderID]; ok {
		return fl, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) UpdateNameAndPath(context.Context, uuid.UUID, uuid.UUID, string, string) error {
	return nil
}
func (f *fakeRepo) UpdateParentAndPath(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID, string) error {
	return nil
}
func (f *fakeRepo) RenameWithDescendants(context.Context, uuid.UUID, uuid.UUID, string, string, string) error {
	return nil
}
func (f *fakeRepo) MoveWithDescendants(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID, string, string) error {
	return nil
}
func (f *fakeRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID) error        { return nil }
func (f *fakeRepo) SoftDeleteSubtree(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeRepo) ListChildren(context.Context, uuid.UUID, *uuid.UUID) ([]*Folder, error) {
	return nil, nil
}
func (f *fakeRepo) ListDescendants(context.Context, uuid.UUID, uuid.UUID) ([]*Folder, error) {
	return nil, nil
}
func (f *fakeRepo) GetAncestors(context.Context, uuid.UUID, uuid.UUID) ([]*Folder, error) {
	return nil, nil
}

// fakeResolver stands in for workspace.Service.GetDefaultEncryptionMode.
type fakeResolver struct {
	mode   string
	err    error
	called int
}

func (r *fakeResolver) GetDefaultEncryptionMode(context.Context, uuid.UUID) (string, error) {
	r.called++
	return r.mode, r.err
}

// TestCreateRootFolder_NoResolverDefaultsManaged: with no resolver
// wired (dev/test), a root folder created without an explicit mode
// keeps the historical managed-encrypted default.
func TestCreateRootFolder_NoResolverDefaultsManaged(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo)
	f, err := svc.Create(context.Background(), uuid.New(), nil, "docs", uuid.New())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if f.EncryptionMode != EncryptionManagedEncrypted {
		t.Errorf("mode = %q, want %q", f.EncryptionMode, EncryptionManagedEncrypted)
	}
}

// TestCreateRootFolder_ResolverStrictZK: a workspace whose default is
// strict_zk produces strict_zk root folders when no mode is supplied.
func TestCreateRootFolder_ResolverStrictZK(t *testing.T) {
	repo := &fakeRepo{}
	res := &fakeResolver{mode: EncryptionStrictZK}
	svc := NewService(repo, WithWorkspaceDefaults(res))
	f, err := svc.Create(context.Background(), uuid.New(), nil, "secret", uuid.New())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if f.EncryptionMode != EncryptionStrictZK {
		t.Errorf("mode = %q, want %q", f.EncryptionMode, EncryptionStrictZK)
	}
	if res.called != 1 {
		t.Errorf("resolver called %d times, want 1", res.called)
	}
}

// TestCreateRootFolder_ExplicitModeWinsOverResolver: an explicit mode
// from the caller is never overridden by the workspace default, so the
// resolver isn't even consulted.
func TestCreateRootFolder_ExplicitModeWinsOverResolver(t *testing.T) {
	repo := &fakeRepo{}
	res := &fakeResolver{mode: EncryptionStrictZK}
	svc := NewService(repo, WithWorkspaceDefaults(res))
	f, err := svc.CreateWithMode(context.Background(), uuid.New(), nil, "docs", EncryptionManagedEncrypted, uuid.New())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if f.EncryptionMode != EncryptionManagedEncrypted {
		t.Errorf("mode = %q, want %q", f.EncryptionMode, EncryptionManagedEncrypted)
	}
	if res.called != 0 {
		t.Errorf("resolver consulted despite explicit mode (called=%d)", res.called)
	}
}

// TestCreateRootFolder_ResolverErrorFailsClosed: a workspace-lookup
// error must propagate, not silently downgrade to managed-encrypted —
// a strict_zk workspace must never get a less-private folder due to a
// transient read error.
func TestCreateRootFolder_ResolverErrorFailsClosed(t *testing.T) {
	repo := &fakeRepo{}
	sentinel := errors.New("db down")
	res := &fakeResolver{err: sentinel}
	svc := NewService(repo, WithWorkspaceDefaults(res))
	_, err := svc.Create(context.Background(), uuid.New(), nil, "docs", uuid.New())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the resolver error to propagate, got %v", err)
	}
	if repo.created != nil {
		t.Errorf("folder was created despite a failed default-mode lookup")
	}
}

// TestCreateRootFolder_ResolverInvalidModeRejected: a corrupt default
// (outside the recognised set) must be rejected rather than persisted.
func TestCreateRootFolder_ResolverInvalidModeRejected(t *testing.T) {
	repo := &fakeRepo{}
	res := &fakeResolver{mode: "garbage"}
	svc := NewService(repo, WithWorkspaceDefaults(res))
	_, err := svc.Create(context.Background(), uuid.New(), nil, "docs", uuid.New())
	if !errors.Is(err, ErrInvalidEncryptionMode) {
		t.Fatalf("expected ErrInvalidEncryptionMode, got %v", err)
	}
	if repo.created != nil {
		t.Errorf("folder was created with an invalid default mode")
	}
}

// TestCreateChildFolder_InheritsParentNotResolver: child folders always
// inherit their parent's mode; the workspace default resolver only
// governs root folders and must not be consulted for children.
func TestCreateChildFolder_InheritsParentNotResolver(t *testing.T) {
	wsID := uuid.New()
	parentID := uuid.New()
	repo := &fakeRepo{byID: map[uuid.UUID]*Folder{
		parentID: {ID: parentID, WorkspaceID: wsID, Path: "/p/", EncryptionMode: EncryptionManagedEncrypted},
	}}
	res := &fakeResolver{mode: EncryptionStrictZK}
	svc := NewService(repo, WithWorkspaceDefaults(res))
	f, err := svc.Create(context.Background(), wsID, &parentID, "child", uuid.New())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if f.EncryptionMode != EncryptionManagedEncrypted {
		t.Errorf("child mode = %q, want inherited %q", f.EncryptionMode, EncryptionManagedEncrypted)
	}
	if res.called != 0 {
		t.Errorf("resolver consulted for a child folder (called=%d)", res.called)
	}
}
