package gc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/file"
)

// TestGCAllRejectsNilDependencies guards the documented invariant
// that GCAll fails fast rather than panicking when the GCService is
// constructed without a pool or file service. Both the standalone
// binary and the worker wrap construction in a goroutine where a
// panic would crash the whole process — the early-error path keeps
// failure visible in `kubectl logs --previous` even when the
// scheduler hasn't yet recorded a metric for the failed run.
func TestGCAllRejectsNilDependencies(t *testing.T) {
	t.Run("nil_receiver", func(t *testing.T) {
		var s *GCService
		if _, err := s.GCAll(context.Background()); err == nil {
			t.Fatalf("expected error on nil receiver, got nil")
		}
	})
	t.Run("nil_pool", func(t *testing.T) {
		s := &GCService{}
		if _, err := s.GCAll(context.Background()); err == nil {
			t.Fatalf("expected error on nil pool, got nil")
		}
	})
}

// TestGCWorkspaceUsesConfiguredClock pins the WithClock option
// against a fixed reference time so the package-level TTL math is
// deterministic in tests (otherwise the cutoff would walk with the
// suite's wall clock and the "older than 7 days" predicate would
// behave differently in CI than locally).
func TestGCWorkspaceUsesConfiguredClock(t *testing.T) {
	repo := &fakeFileRepo{}
	svc := file.NewService(repo)
	fixed := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	gcSvc := New(nil, svc, nil,
		WithTTL(24*time.Hour),
		WithClock(func() time.Time { return fixed }),
	)
	wsID := uuid.New()
	_, err := gcSvc.GCWorkspace(context.Background(), wsID)
	if err != nil {
		t.Fatalf("GCWorkspace returned error: %v", err)
	}
	wantCutoff := fixed.Add(-24 * time.Hour)
	if !repo.lastCutoff.Equal(wantCutoff) {
		t.Fatalf("cutoff = %v, want %v", repo.lastCutoff, wantCutoff)
	}
}

// TestGCWorkspaceDeletesOrphansAndObjects verifies the end-to-end
// per-row reclaim: each orphan returned by the list query gets a
// DeleteObject call followed by a DeletePendingOrphan. Both the
// object and the row counters increment.
func TestGCWorkspaceDeletesOrphansAndObjects(t *testing.T) {
	wsID := uuid.New()
	orphans := []*file.PendingOrphan{
		{FileID: uuid.New(), ObjectKey: "ws/file/v1", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)},
		{FileID: uuid.New(), ObjectKey: "ws/file/v2", CreatedAt: time.Now().Add(-9 * 24 * time.Hour)},
	}
	repo := &fakeFileRepo{listResult: orphans}
	svc := file.NewService(repo)

	deleter := &fakeDeleter{}
	resolver := func(ctx context.Context, workspaceID uuid.UUID) StorageDeleter {
		if workspaceID != wsID {
			t.Fatalf("resolver got unexpected workspace_id %s, want %s", workspaceID, wsID)
		}
		return deleter
	}

	gcSvc := New(nil, svc, resolver, WithTTL(time.Hour))
	res, err := gcSvc.GCWorkspace(context.Background(), wsID)
	if err != nil {
		t.Fatalf("GCWorkspace returned error: %v", err)
	}
	if res.Found != 2 || res.RowsDeleted != 2 || res.ObjectsDeleted != 2 {
		t.Fatalf("result = %+v, want found=2 deleted=2 objects=2", res)
	}
	if got := len(deleter.calls); got != 2 {
		t.Fatalf("deleter calls = %d, want 2", got)
	}
	if got := len(repo.deleteCalls); got != 2 {
		t.Fatalf("DeletePendingOrphan calls = %d, want 2", got)
	}
}

// TestGCWorkspaceContinuesPastObjectDeleteFailure verifies the
// best-effort contract: a transient DeleteObject failure does NOT
// abort the row delete; the row is still reclaimed (the row is
// authoritative for visibility, the object delete is opportunistic).
// ObjectsDeleted < RowsDeleted is the operator-visible signal in
// /metrics that the storage path is unhealthy.
func TestGCWorkspaceContinuesPastObjectDeleteFailure(t *testing.T) {
	orphans := []*file.PendingOrphan{
		{FileID: uuid.New(), ObjectKey: "ws/file/v1", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)},
	}
	repo := &fakeFileRepo{listResult: orphans}
	svc := file.NewService(repo)
	deleter := &fakeDeleter{err: errors.New("storage 503 transient")}
	resolver := func(_ context.Context, _ uuid.UUID) StorageDeleter { return deleter }

	gcSvc := New(nil, svc, resolver, WithTTL(time.Hour))
	res, err := gcSvc.GCWorkspace(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("GCWorkspace returned error: %v", err)
	}
	if res.Found != 1 || res.RowsDeleted != 1 || res.ObjectsDeleted != 0 {
		t.Fatalf("result = %+v, want found=1 deleted=1 objects=0", res)
	}
}

// TestGCWorkspaceTreatsConfirmRaceAsBenign verifies the
// predicate-guarded DELETE contract: if a concurrent ConfirmUpload
// landed between the list and the delete, current_version_id is now
// non-NULL, the DELETE matches zero rows -> ErrNotFound. The GC
// reconciler treats that as a benign race (the row is no longer an
// orphan) rather than a per-row failure that would inflate the
// error counter.
//
// Critically, the GC also MUST NOT delete the S3 object in this
// case: the racing confirm just promoted the row to a real version
// pointing at exactly that key, so deleting the bytes here would
// strand a confirmed file. The row-delete-first ordering inside
// GCWorkspace makes the race detection a precondition for the
// storage delete, which is the architectural property this test
// pins.
func TestGCWorkspaceTreatsConfirmRaceAsBenign(t *testing.T) {
	orphans := []*file.PendingOrphan{
		{FileID: uuid.New(), ObjectKey: "ws/file/v1", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)},
	}
	repo := &fakeFileRepo{listResult: orphans, deleteErr: file.ErrNotFound}
	svc := file.NewService(repo)
	deleter := &fakeDeleter{}
	resolver := func(_ context.Context, _ uuid.UUID) StorageDeleter { return deleter }

	gcSvc := New(nil, svc, resolver, WithTTL(time.Hour))
	res, err := gcSvc.GCWorkspace(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("GCWorkspace returned error: %v", err)
	}
	if res.RowsDeleted != 0 {
		t.Fatalf("RowsDeleted = %d, want 0 (race treated as benign)", res.RowsDeleted)
	}
	// Critical: the object MUST NOT have been deleted. The raced
	// confirm now owns this key and the file row points at it.
	if res.ObjectsDeleted != 0 {
		t.Fatalf("ObjectsDeleted = %d, want 0 (race must skip storage delete)", res.ObjectsDeleted)
	}
	if len(deleter.calls) != 0 {
		t.Fatalf("DeleteObject was called %d times; expected 0 because the row predicate guard fired", len(deleter.calls))
	}
}

// TestGCWorkspaceSkipsObjectDeleteWhenResolverReturnsNil verifies
// the "storage unconfigured" path: when the resolver returns nil
// (suspended tenant, missing credentials), the GC still reclaims
// the metadata row but skips the object delete. ObjectsDeleted = 0
// is the expected behaviour, not an error.
func TestGCWorkspaceSkipsObjectDeleteWhenResolverReturnsNil(t *testing.T) {
	orphans := []*file.PendingOrphan{
		{FileID: uuid.New(), ObjectKey: "ws/file/v1", CreatedAt: time.Now().Add(-10 * 24 * time.Hour)},
	}
	repo := &fakeFileRepo{listResult: orphans}
	svc := file.NewService(repo)
	resolver := func(_ context.Context, _ uuid.UUID) StorageDeleter { return nil }

	gcSvc := New(nil, svc, resolver, WithTTL(time.Hour))
	res, err := gcSvc.GCWorkspace(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("GCWorkspace returned error: %v", err)
	}
	if res.RowsDeleted != 1 || res.ObjectsDeleted != 0 {
		t.Fatalf("result = %+v, want deleted=1 objects=0", res)
	}
}

// fakeFileRepo is a hand-rolled stub for file.Repository scoped
// down to the methods GCService needs. Other interface methods
// panic to keep the surface area visible: if a future GC change
// starts calling AddTag or RenameFile, the test will tell us
// loudly rather than silently passing.
type fakeFileRepo struct {
	listResult []*file.PendingOrphan
	lastCutoff time.Time

	deleteCalls []uuid.UUID
	deleteErr   error
}

func (r *fakeFileRepo) ListPendingUploadOrphans(_ context.Context, _ uuid.UUID, olderThan time.Time, _ int) ([]*file.PendingOrphan, error) {
	r.lastCutoff = olderThan
	return r.listResult, nil
}

func (r *fakeFileRepo) DeletePendingOrphan(_ context.Context, _ uuid.UUID, fileID uuid.UUID) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.deleteCalls = append(r.deleteCalls, fileID)
	return nil
}

// Unused-by-GC methods: panic on call so a future caller doesn't
// silently pull this fake into a code path it wasn't designed for.
func (r *fakeFileRepo) CreateFile(context.Context, *file.File) error { panic("unused") }
func (r *fakeFileRepo) GetFileByID(context.Context, uuid.UUID, uuid.UUID) (*file.File, error) {
	panic("unused")
}
func (r *fakeFileRepo) UpdateFile(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID) error {
	panic("unused")
}
func (r *fakeFileRepo) RenameFile(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("unused")
}
func (r *fakeFileRepo) DeleteFile(context.Context, uuid.UUID, uuid.UUID) error { panic("unused") }
func (r *fakeFileRepo) MoveFile(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error {
	panic("unused")
}
func (r *fakeFileRepo) UpdateFileSize(context.Context, uuid.UUID, uuid.UUID, int64) error {
	panic("unused")
}
func (r *fakeFileRepo) ListFilesByFolder(context.Context, uuid.UUID, uuid.UUID) ([]*file.File, error) {
	panic("unused")
}
func (r *fakeFileRepo) SetPendingUploadObjectKey(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("unused")
}
func (r *fakeFileRepo) CreateFileVersion(context.Context, uuid.UUID, *file.FileVersion) error {
	panic("unused")
}
func (r *fakeFileRepo) CreateVersionAndSetCurrent(context.Context, uuid.UUID, *file.FileVersion) error {
	panic("unused")
}
func (r *fakeFileRepo) ConfirmVersion(context.Context, uuid.UUID, *file.FileVersion) (bool, error) {
	panic("unused")
}
func (r *fakeFileRepo) ListVersions(context.Context, uuid.UUID, uuid.UUID) ([]*file.FileVersion, error) {
	panic("unused")
}
func (r *fakeFileRepo) GetVersionByID(context.Context, uuid.UUID, uuid.UUID) (*file.FileVersion, error) {
	panic("unused")
}
func (r *fakeFileRepo) SetCurrentVersion(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error {
	panic("unused")
}
func (r *fakeFileRepo) AddTag(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string) (*file.Tag, error) {
	panic("unused")
}
func (r *fakeFileRepo) RemoveTag(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("unused")
}
func (r *fakeFileRepo) ListTagsByFile(context.Context, uuid.UUID, uuid.UUID) ([]*file.Tag, error) {
	panic("unused")
}
func (r *fakeFileRepo) ListTagsByWorkspace(context.Context, uuid.UUID) ([]*file.Tag, error) {
	panic("unused")
}

// fakeDeleter records DeleteObject calls and optionally returns a
// stubbed error to exercise the best-effort path.
type fakeDeleter struct {
	calls []string
	err   error
}

func (d *fakeDeleter) DeleteObject(_ context.Context, key string) error {
	d.calls = append(d.calls, key)
	return d.err
}
