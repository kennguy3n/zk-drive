package document

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// fakeFolderLookup is an in-memory FolderLookup for service-level
// tests. It does not touch Postgres so the capability + service-
// flow tests stay hermetic.
type fakeFolderLookup struct {
	byID map[uuid.UUID]*folder.Folder
}

func (f *fakeFolderLookup) GetByID(_ context.Context, _ uuid.UUID, folderID uuid.UUID) (*folder.Folder, error) {
	if got, ok := f.byID[folderID]; ok {
		return got, nil
	}
	return nil, folder.ErrNotFound
}

// fakeRepo is the in-memory Repository used by service tests. It
// captures the most recent call args + lets tests stub return values
// without spinning up Postgres / RLS plumbing.
type fakeRepo struct {
	createErr           error
	createdDoc          *Document
	docs                map[uuid.UUID]*Document
	getErr              error
	updateNameErr       error
	updateNameDoc       *Document
	updateCollabErr     error
	updateCollabDoc     *Document
	deleteErr           error
	listByFolder        []*Document
	listByFolderErr     error
	appendDeltaErr      error
	appendedDelta       *Delta
	listDeltas          []*Delta
	listDeltasErr       error
	countDeltasAfter    int64
	countDeltasAfterErr error
	replaceSnapshotDoc  *Document
	replaceSnapshotErr  error
	replaceSnapshotArgs struct {
		yState         []byte
		yStateVector   []byte
		upToSeq        int64
	}
}

func (r *fakeRepo) Create(_ context.Context, d *Document) error {
	if r.createErr != nil {
		return r.createErr
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	d.CreatedAt = time.Now().UTC()
	d.UpdatedAt = d.CreatedAt
	r.createdDoc = d
	if r.docs == nil {
		r.docs = map[uuid.UUID]*Document{}
	}
	r.docs[d.ID] = d
	return nil
}

func (r *fakeRepo) GetByID(_ context.Context, _, id uuid.UUID) (*Document, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if d, ok := r.docs[id]; ok {
		return d, nil
	}
	return nil, ErrNotFound
}

func (r *fakeRepo) UpdateName(_ context.Context, _, _ uuid.UUID, name string) (*Document, error) {
	if r.updateNameErr != nil {
		return nil, r.updateNameErr
	}
	if r.updateNameDoc != nil {
		r.updateNameDoc.Name = name
	}
	return r.updateNameDoc, nil
}

func (r *fakeRepo) UpdateCollabMode(_ context.Context, _, _ uuid.UUID, mode string) (*Document, error) {
	if r.updateCollabErr != nil {
		return nil, r.updateCollabErr
	}
	if r.updateCollabDoc != nil {
		r.updateCollabDoc.CollabMode = mode
	}
	return r.updateCollabDoc, nil
}

func (r *fakeRepo) SoftDelete(_ context.Context, _, _ uuid.UUID) error {
	return r.deleteErr
}

func (r *fakeRepo) ListByFolder(_ context.Context, _, _ uuid.UUID) ([]*Document, error) {
	return r.listByFolder, r.listByFolderErr
}

func (r *fakeRepo) AppendDelta(_ context.Context, d *Delta) error {
	if r.appendDeltaErr != nil {
		return r.appendDeltaErr
	}
	d.Seq = 1
	d.CreatedAt = time.Now().UTC()
	r.appendedDelta = d
	return nil
}

func (r *fakeRepo) ListDeltas(_ context.Context, _, _ uuid.UUID, _ int64, _ int) ([]*Delta, error) {
	return r.listDeltas, r.listDeltasErr
}

func (r *fakeRepo) CountDeltasAfter(_ context.Context, _, _ uuid.UUID, _ int64) (int64, error) {
	return r.countDeltasAfter, r.countDeltasAfterErr
}

func (r *fakeRepo) ReplaceSnapshot(_ context.Context, _, _ uuid.UUID, yState, yStateVector []byte, upToSeq int64) (*Document, error) {
	if r.replaceSnapshotErr != nil {
		return nil, r.replaceSnapshotErr
	}
	r.replaceSnapshotArgs.yState = yState
	r.replaceSnapshotArgs.yStateVector = yStateVector
	r.replaceSnapshotArgs.upToSeq = upToSeq
	return r.replaceSnapshotDoc, nil
}

func (r *fakeRepo) GetSnapshotBundle(ctx context.Context, workspaceID, documentID uuid.UUID, _ int) (*Document, []*Delta, error) {
	// Fake conveniently composes GetByID + ListDeltas — production
	// PostgresRepository runs both in a REPEATABLE READ tx; the
	// in-memory fake has no concurrency to worry about.
	d, err := r.GetByID(ctx, workspaceID, documentID)
	if err != nil {
		return nil, nil, err
	}
	deltas, err := r.ListDeltas(ctx, workspaceID, documentID, d.YStateSeqFloor, 0)
	if err != nil {
		return nil, nil, err
	}
	return d, deltas, nil
}

func newServiceFixture(t *testing.T, folderMode string) (*Service, *fakeRepo, *folder.Folder) {
	t.Helper()
	parent := &folder.Folder{
		ID:             uuid.New(),
		WorkspaceID:    uuid.New(),
		Name:           "docs",
		Path:           "/docs/",
		EncryptionMode: folderMode,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	repo := &fakeRepo{}
	lookup := &fakeFolderLookup{byID: map[uuid.UUID]*folder.Folder{parent.ID: parent}}
	return NewService(repo, lookup), repo, parent
}

// TestService_Create_ManagedDefault verifies a managed_encrypted
// folder gets rich_presence by default when the user doesn't pick
// a collab mode.
func TestService_Create_ManagedDefault(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	doc, gotParent, err := svc.Create(context.Background(), CreateInput{
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		Name:        "Q4 planning",
		CreatedBy:   uuid.New(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc.CollabMode != CollabModeRichPresence {
		t.Fatalf("default mode = %q, want %q", doc.CollabMode, CollabModeRichPresence)
	}
	if gotParent.EncryptionMode != folder.EncryptionManagedEncrypted {
		t.Fatalf("Create did not return parent folder")
	}
	if repo.createdDoc == nil {
		t.Fatal("Create did not call repo.Create")
	}
}

// TestService_Create_StrictDefault verifies strict_zk folders get
// markdown by default — the only allowed mode.
func TestService_Create_StrictDefault(t *testing.T) {
	t.Parallel()
	svc, _, parent := newServiceFixture(t, folder.EncryptionStrictZK)
	doc, _, err := svc.Create(context.Background(), CreateInput{
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		Name:        "Vault notes",
		CreatedBy:   uuid.New(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc.CollabMode != CollabModeMarkdown {
		t.Fatalf("default mode = %q, want %q", doc.CollabMode, CollabModeMarkdown)
	}
}

// TestService_Create_StrictRejectsRich is the critical privacy-
// boundary test: a strict_zk folder MUST refuse a 'rich' or
// 'rich_presence' document creation because those modes require
// the server to decrypt the Yjs state.
func TestService_Create_StrictRejectsRich(t *testing.T) {
	t.Parallel()
	svc, _, parent := newServiceFixture(t, folder.EncryptionStrictZK)
	cases := []string{CollabModeRich, CollabModeRichPresence}
	for _, mode := range cases {
		t.Run(mode, func(t *testing.T) {
			_, _, err := svc.Create(context.Background(), CreateInput{
				WorkspaceID: parent.WorkspaceID,
				FolderID:    parent.ID,
				Name:        "doc",
				CollabMode:  mode,
				CreatedBy:   uuid.New(),
			})
			if !errors.Is(err, ErrCollabModeNotAllowed) {
				t.Fatalf("expected ErrCollabModeNotAllowed, got %v", err)
			}
		})
	}
}

// TestService_Create_RejectsDisabledTombstone verifies the user
// can't directly set the 'disabled' tombstone — it's reserved for
// the service.
func TestService_Create_RejectsDisabledTombstone(t *testing.T) {
	t.Parallel()
	svc, _, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	_, _, err := svc.Create(context.Background(), CreateInput{
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		Name:        "doc",
		CollabMode:  CollabModeDisabled,
		CreatedBy:   uuid.New(),
	})
	if !errors.Is(err, ErrCollabModeNotAllowed) {
		t.Fatalf("expected ErrCollabModeNotAllowed, got %v", err)
	}
}

// TestService_Create_RejectsInvalidMode pins the "unknown mode"
// path. Service should not query the folder repo on a malformed
// input.
func TestService_Create_RejectsInvalidMode(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	_, _, err := svc.Create(context.Background(), CreateInput{
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		Name:        "doc",
		CollabMode:  "not-a-mode",
		CreatedBy:   uuid.New(),
	})
	if !errors.Is(err, ErrInvalidCollabMode) {
		t.Fatalf("expected ErrInvalidCollabMode, got %v", err)
	}
	if repo.createdDoc != nil {
		t.Fatal("repo.Create should not have been called on invalid mode")
	}
}

// TestService_Create_RejectsEmptyName covers the name validator.
func TestService_Create_RejectsEmptyName(t *testing.T) {
	t.Parallel()
	svc, _, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	cases := []string{"", "   ", strings.Repeat("a", MaxNameBytes+1), "has/slash"}
	for _, name := range cases {
		_, _, err := svc.Create(context.Background(), CreateInput{
			WorkspaceID: parent.WorkspaceID,
			FolderID:    parent.ID,
			Name:        name,
			CreatedBy:   uuid.New(),
		})
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name=%q: expected ErrInvalidName, got %v", name, err)
		}
	}
}

// TestService_GetByID_ReturnsParentFolder proves GetByID returns
// the parent folder so the caller can derive live capability without
// a second round-trip. The folder is live-inherited (not cached on
// the document), which is the whole point of the privacy boundary
// being immutable at the folder layer.
func TestService_GetByID_ReturnsParentFolder(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	doc := &Document{
		ID:          docID,
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		Name:        "doc",
		CollabMode:  CollabModeRichPresence,
	}
	repo.docs = map[uuid.UUID]*Document{docID: doc}

	gotDoc, gotParent, err := svc.GetByID(context.Background(), parent.WorkspaceID, docID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if gotDoc.ID != docID {
		t.Fatalf("returned wrong document id: %s vs %s", gotDoc.ID, docID)
	}
	if gotParent == nil || gotParent.ID != parent.ID {
		t.Fatalf("expected parent folder %s, got %+v", parent.ID, gotParent)
	}
	if gotParent.EncryptionMode != folder.EncryptionManagedEncrypted {
		t.Fatalf("parent encryption mode = %q, want %q",
			gotParent.EncryptionMode, folder.EncryptionManagedEncrypted)
	}
	// Capability is derived live from the returned parent folder.
	wantCap := Capability{
		ServerSnapshotAllowed: true,
		RichExtensionsAllowed: true,
		PresenceAllowed:       true,
	}
	if got := ResolveCapability(gotParent.EncryptionMode); got != wantCap {
		t.Fatalf("derived capability mismatch: got %+v, want %+v", got, wantCap)
	}
}

// TestService_SetCollabMode_RejectsDisabled covers the "user can't
// directly select disabled" rule.
func TestService_SetCollabMode_RejectsDisabled(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	repo.docs = map[uuid.UUID]*Document{docID: {
		ID:          docID,
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		CollabMode:  CollabModeRich,
	}}
	_, err := svc.SetCollabMode(context.Background(), parent.WorkspaceID, docID, CollabModeDisabled)
	if !errors.Is(err, ErrInvalidCollabMode) {
		t.Fatalf("expected ErrInvalidCollabMode, got %v", err)
	}
}

// TestService_SetCollabMode_RejectsStrictRich proves the privacy
// boundary holds on the update path too — not just create.
func TestService_SetCollabMode_RejectsStrictRich(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionStrictZK)
	docID := uuid.New()
	repo.docs = map[uuid.UUID]*Document{docID: {
		ID:          docID,
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		CollabMode:  CollabModeMarkdown,
	}}
	_, err := svc.SetCollabMode(context.Background(), parent.WorkspaceID, docID, CollabModeRichPresence)
	if !errors.Is(err, ErrCollabModeNotAllowed) {
		t.Fatalf("expected ErrCollabModeNotAllowed, got %v", err)
	}
}

// TestService_AppendDelta_RejectsOversizedPayload covers the
// pre-INSERT guard so the client gets a clean 4xx instead of a
// Postgres CHECK violation.
func TestService_AppendDelta_RejectsOversizedPayload(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	repo.docs = map[uuid.UUID]*Document{docID: {
		ID:          docID,
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		CollabMode:  CollabModeRichPresence,
	}}
	payload := make([]byte, MaxDeltaPayloadBytes+1)
	_, err := svc.AppendDelta(context.Background(), AppendDeltaInput{
		WorkspaceID:  parent.WorkspaceID,
		DocumentID:   docID,
		Payload:      payload,
		AuthorUserID: uuid.New(),
	})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
}

// TestService_AppendDelta_RejectsEmptyPayload pins the "empty
// payload" path.
func TestService_AppendDelta_RejectsEmptyPayload(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	repo.docs = map[uuid.UUID]*Document{docID: {
		ID:          docID,
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		CollabMode:  CollabModeRichPresence,
	}}
	_, err := svc.AppendDelta(context.Background(), AppendDeltaInput{
		WorkspaceID:  parent.WorkspaceID,
		DocumentID:   docID,
		Payload:      nil,
		AuthorUserID: uuid.New(),
	})
	if !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("expected ErrEmptyPayload, got %v", err)
	}
}

// TestService_AppendDelta_RejectsDisabledDocument proves a document
// in the tombstone state refuses new deltas — the user has to
// re-enable collab first.
func TestService_AppendDelta_RejectsDisabledDocument(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	repo.docs = map[uuid.UUID]*Document{docID: {
		ID:          docID,
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		CollabMode:  CollabModeDisabled,
	}}
	_, err := svc.AppendDelta(context.Background(), AppendDeltaInput{
		WorkspaceID:  parent.WorkspaceID,
		DocumentID:   docID,
		Payload:      []byte{1, 2, 3},
		AuthorUserID: uuid.New(),
	})
	if !errors.Is(err, ErrCollabModeNotAllowed) {
		t.Fatalf("expected ErrCollabModeNotAllowed, got %v", err)
	}
}

// TestService_AppendDelta_CompactionHint verifies the compaction
// signal fires when the pending-delta count crosses the threshold.
func TestService_AppendDelta_CompactionHint(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	repo.docs = map[uuid.UUID]*Document{docID: {
		ID:          docID,
		WorkspaceID: parent.WorkspaceID,
		FolderID:    parent.ID,
		CollabMode:  CollabModeRichPresence,
	}}
	repo.countDeltasAfter = CompactionThreshold

	res, err := svc.AppendDelta(context.Background(), AppendDeltaInput{
		WorkspaceID:  parent.WorkspaceID,
		DocumentID:   docID,
		Payload:      []byte{1, 2, 3},
		AuthorUserID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("AppendDelta: %v", err)
	}
	if !res.CompactionDue {
		t.Fatalf("expected compaction hint when count >= threshold (got count=%d)", res.PendingDeltaCount)
	}
}

// TestService_Compact_RejectsNonProgressingFold ensures the snapshot
// floor cannot regress — a buggy fold callback that returns a
// stale upToSeq must surface an error rather than corrupt state.
func TestService_Compact_RejectsNonProgressingFold(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	repo.docs = map[uuid.UUID]*Document{docID: {
		ID:             docID,
		WorkspaceID:    parent.WorkspaceID,
		FolderID:       parent.ID,
		YStateSeqFloor: 42,
	}}
	// Single tail delta with seq=43 (above the current floor).
	repo.listDeltas = []*Delta{{
		DocumentID: docID,
		Seq:        43,
		Payload:    []byte{1},
	}}

	// Fold returns a non-progressing upToSeq (== current floor).
	_, err := svc.Compact(context.Background(), parent.WorkspaceID, docID, func(_, _ []byte, _ []*Delta) ([]byte, []byte, int64, error) {
		return []byte("snap"), []byte("vec"), 42, nil
	})
	if err == nil || !strings.Contains(err.Error(), "non-progressing") {
		t.Fatalf("expected non-progressing error, got %v", err)
	}
}

// TestService_Compact_NoTailIsNoOp verifies an idempotent retry
// with an empty tail returns the existing document unchanged.
func TestService_Compact_NoTailIsNoOp(t *testing.T) {
	t.Parallel()
	svc, repo, parent := newServiceFixture(t, folder.EncryptionManagedEncrypted)
	docID := uuid.New()
	existing := &Document{
		ID:             docID,
		WorkspaceID:    parent.WorkspaceID,
		FolderID:       parent.ID,
		YStateSeqFloor: 100,
	}
	repo.docs = map[uuid.UUID]*Document{docID: existing}
	repo.listDeltas = nil

	got, err := svc.Compact(context.Background(), parent.WorkspaceID, docID, func(_, _ []byte, _ []*Delta) ([]byte, []byte, int64, error) {
		t.Fatal("fold callback should NOT be called when tail is empty")
		return nil, nil, 0, nil
	})
	if err != nil {
		t.Fatalf("Compact (no-op): %v", err)
	}
	if got != existing {
		t.Fatalf("Compact should return the existing document unchanged when tail is empty")
	}
}
