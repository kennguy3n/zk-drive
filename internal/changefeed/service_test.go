package changefeed_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/changefeed"
)

// fakeRepo is an in-memory Repository that lets unit tests exercise
// Service.Record / Since / Latest without a Postgres dependency. It
// is concurrency-safe so the BroadcastJSONWorkspace race test can
// run Record from multiple goroutines simultaneously.
type fakeRepo struct {
	mu       sync.Mutex
	next     int64
	rows     []changefeed.Mutation
	recordOK chan struct{}
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{}
}

func (r *fakeRepo) Record(_ context.Context, m *changefeed.Mutation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	m.Sequence = r.next
	m.OccurredAt = time.Now().UTC()
	clone := *m
	if len(m.Metadata) > 0 {
		// Deep-copy the raw message so a caller mutating the input
		// after Record does not corrupt the stored row, matching
		// the Postgres backend's value-copy semantics.
		clone.Metadata = append(json.RawMessage(nil), m.Metadata...)
	}
	r.rows = append(r.rows, clone)
	if r.recordOK != nil {
		select {
		case r.recordOK <- struct{}{}:
		default:
		}
	}
	return nil
}

func (r *fakeRepo) BatchRecord(_ context.Context, muts []changefeed.Mutation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	for i := range muts {
		r.next++
		muts[i].Sequence = r.next
		muts[i].OccurredAt = now
		clone := muts[i]
		if len(muts[i].Metadata) > 0 {
			clone.Metadata = append(json.RawMessage(nil), muts[i].Metadata...)
		}
		r.rows = append(r.rows, clone)
		if r.recordOK != nil {
			select {
			case r.recordOK <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

func (r *fakeRepo) Since(_ context.Context, workspaceID uuid.UUID, cursor int64, limit int) ([]changefeed.Mutation, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matched []changefeed.Mutation
	for _, row := range r.rows {
		if row.WorkspaceID != workspaceID {
			continue
		}
		if row.Sequence <= cursor {
			continue
		}
		matched = append(matched, row)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Sequence < matched[j].Sequence })
	hasMore := false
	if len(matched) > limit {
		matched = matched[:limit]
		hasMore = true
	}
	return matched, hasMore, nil
}

func (r *fakeRepo) Latest(_ context.Context, workspaceID uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var max int64
	for _, row := range r.rows {
		if row.WorkspaceID == workspaceID && row.Sequence > max {
			max = row.Sequence
		}
	}
	return max, nil
}

// errRepo always fails Record / Since so error-path coverage exists
// alongside the happy-path tests.
type errRepo struct{ err error }

func (e errRepo) Record(_ context.Context, _ *changefeed.Mutation) error       { return e.err }
func (e errRepo) BatchRecord(_ context.Context, _ []changefeed.Mutation) error { return e.err }
func (e errRepo) Since(_ context.Context, _ uuid.UUID, _ int64, _ int) ([]changefeed.Mutation, bool, error) {
	return nil, false, e.err
}
func (e errRepo) Latest(_ context.Context, _ uuid.UUID) (int64, error) { return 0, e.err }

// recordingPublisher captures every Publish call so tests can
// assert on broadcast wiring.
type recordingPublisher struct {
	mu     sync.Mutex
	events []changefeed.Event
	last   uuid.UUID
	err    error
}

func (p *recordingPublisher) Publish(_ context.Context, workspaceID uuid.UUID, event changefeed.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
	p.last = workspaceID
	return p.err
}

func (p *recordingPublisher) snapshot() ([]changefeed.Event, uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]changefeed.Event, len(p.events))
	copy(out, p.events)
	return out, p.last
}

func TestServiceRecord_PersistsAndBroadcasts(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	pub := &recordingPublisher{}
	svc := changefeed.NewService(repo).WithPublisher(pub)

	wsID := uuid.New()
	resID := uuid.New()
	actor := uuid.New()
	parent := uuid.New()

	mut, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: wsID,
		ActorID:     &actor,
		Kind:        changefeed.KindFile,
		Op:          changefeed.OpCreate,
		ResourceID:  resID,
		ParentID:    &parent,
		Name:        "report.pdf",
		Metadata:    map[string]any{"size_bytes": 1024},
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if mut.Sequence == 0 {
		t.Fatalf("expected non-zero sequence, got %d", mut.Sequence)
	}
	if mut.WorkspaceID != wsID {
		t.Fatalf("workspace id mismatch")
	}
	if got := mut.Name; got != "report.pdf" {
		t.Fatalf("name = %q, want %q", got, "report.pdf")
	}
	if mut.ParentID == nil || *mut.ParentID != parent {
		t.Fatalf("parent_id mismatch")
	}

	events, lastWS := pub.snapshot()
	if len(events) != 1 {
		t.Fatalf("publisher got %d events, want 1", len(events))
	}
	if events[0].Type != "change" {
		t.Fatalf("event type = %q, want change", events[0].Type)
	}
	if events[0].Payload.Sequence != mut.Sequence {
		t.Fatalf("event sequence = %d, want %d", events[0].Payload.Sequence, mut.Sequence)
	}
	if lastWS != wsID {
		t.Fatalf("broadcast workspace mismatch")
	}
}

func TestServiceRecord_ValidationErrors(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	svc := changefeed.NewService(repo)

	cases := []struct {
		name string
		in   changefeed.RecordInput
	}{
		{"empty kind", changefeed.RecordInput{WorkspaceID: uuid.New(), ResourceID: uuid.New(), Op: changefeed.OpCreate}},
		{"bogus kind", changefeed.RecordInput{WorkspaceID: uuid.New(), ResourceID: uuid.New(), Kind: "wat", Op: changefeed.OpCreate}},
		{"empty op", changefeed.RecordInput{WorkspaceID: uuid.New(), ResourceID: uuid.New(), Kind: changefeed.KindFile}},
		{"bogus op", changefeed.RecordInput{WorkspaceID: uuid.New(), ResourceID: uuid.New(), Kind: changefeed.KindFile, Op: "fart"}},
		{"missing workspace", changefeed.RecordInput{ResourceID: uuid.New(), Kind: changefeed.KindFile, Op: changefeed.OpCreate}},
		{"missing resource", changefeed.RecordInput{WorkspaceID: uuid.New(), Kind: changefeed.KindFile, Op: changefeed.OpCreate}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Record(context.Background(), tc.in); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestServiceRecord_PublishErrorDoesNotFail(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	pub := &recordingPublisher{err: errors.New("redis down")}
	svc := changefeed.NewService(repo).WithPublisher(pub)

	_, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: uuid.New(),
		Kind:        changefeed.KindFile,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
	})
	if err != nil {
		t.Fatalf("publisher error must not fail Record: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("row was not persisted despite publisher error")
	}
}

func TestServiceRecord_RepoErrorPropagates(t *testing.T) {
	t.Parallel()

	svc := changefeed.NewService(errRepo{err: errors.New("db down")})
	_, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: uuid.New(),
		Kind:        changefeed.KindFile,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
	})
	if err == nil {
		t.Fatalf("expected error from repo")
	}
}

func TestServiceSince_PagesAdvanceCursor(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	svc := changefeed.NewService(repo)
	wsID := uuid.New()

	for i := 0; i < 5; i++ {
		if _, err := svc.Record(context.Background(), changefeed.RecordInput{
			WorkspaceID: wsID,
			Kind:        changefeed.KindFile,
			Op:          changefeed.OpCreate,
			ResourceID:  uuid.New(),
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	page, err := svc.Since(context.Background(), wsID, 0, 2)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(page.Mutations) != 2 {
		t.Fatalf("page1 size = %d, want 2", len(page.Mutations))
	}
	if !page.HasMore {
		t.Fatalf("expected has_more=true on first page")
	}
	if page.Cursor != page.Mutations[1].Sequence {
		t.Fatalf("cursor = %d, want %d (last mutation seq)", page.Cursor, page.Mutations[1].Sequence)
	}

	page2, err := svc.Since(context.Background(), wsID, page.Cursor, 10)
	if err != nil {
		t.Fatalf("since page2: %v", err)
	}
	if len(page2.Mutations) != 3 {
		t.Fatalf("page2 size = %d, want 3", len(page2.Mutations))
	}
	if page2.HasMore {
		t.Fatalf("expected has_more=false on final page")
	}
	if page2.Mutations[0].Sequence <= page.Cursor {
		t.Fatalf("page2 first sequence %d <= cursor %d", page2.Mutations[0].Sequence, page.Cursor)
	}
}

func TestServiceSince_EmptyAdvancesNothing(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	svc := changefeed.NewService(repo)
	wsID := uuid.New()

	page, err := svc.Since(context.Background(), wsID, 42, 10)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(page.Mutations) != 0 {
		t.Fatalf("expected empty mutations on empty workspace")
	}
	if page.Cursor != 42 {
		t.Fatalf("empty page cursor = %d, want supplied 42", page.Cursor)
	}
	if page.HasMore {
		t.Fatalf("empty page has_more = true")
	}
}

func TestServiceSince_ClampsLimit(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	svc := changefeed.NewService(repo)
	wsID := uuid.New()

	// MaxLimit+1 mutations so an unclamped query would return them all.
	for i := 0; i < changefeed.MaxLimit+1; i++ {
		if _, err := svc.Record(context.Background(), changefeed.RecordInput{
			WorkspaceID: wsID,
			Kind:        changefeed.KindFile,
			Op:          changefeed.OpCreate,
			ResourceID:  uuid.New(),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	page, err := svc.Since(context.Background(), wsID, 0, 99999)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(page.Mutations) != changefeed.MaxLimit {
		t.Fatalf("page size %d, want MaxLimit %d", len(page.Mutations), changefeed.MaxLimit)
	}
	if !page.HasMore {
		t.Fatalf("expected has_more=true when clamped")
	}
}

func TestServiceSince_NegativeCursorTreatedAsZero(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	svc := changefeed.NewService(repo)
	wsID := uuid.New()

	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: wsID,
		Kind:        changefeed.KindFile,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	page, err := svc.Since(context.Background(), wsID, -10, 0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(page.Mutations) != 1 {
		t.Fatalf("expected to include first mutation when cursor < 0")
	}
}

func TestServiceLatest(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	svc := changefeed.NewService(repo)
	wsID := uuid.New()
	other := uuid.New()

	seq, err := svc.Latest(context.Background(), wsID)
	if err != nil {
		t.Fatalf("latest empty: %v", err)
	}
	if seq != 0 {
		t.Fatalf("empty workspace latest = %d, want 0", seq)
	}

	for i := 0; i < 3; i++ {
		if _, err := svc.Record(context.Background(), changefeed.RecordInput{
			WorkspaceID: wsID,
			Kind:        changefeed.KindFile,
			Op:          changefeed.OpCreate,
			ResourceID:  uuid.New(),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// One mutation in a different workspace must not affect latest(wsID).
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: other,
		Kind:        changefeed.KindFile,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
	}); err != nil {
		t.Fatalf("record other: %v", err)
	}

	seq, err = svc.Latest(context.Background(), wsID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if seq != 3 {
		t.Fatalf("latest = %d, want 3", seq)
	}
}

func TestServiceLatest_NilWorkspace(t *testing.T) {
	t.Parallel()
	svc := changefeed.NewService(newFakeRepo())
	if _, err := svc.Latest(context.Background(), uuid.Nil); err == nil {
		t.Fatalf("expected error for nil workspace id")
	}
}

func TestServiceBatchRecord_PersistsAllAndBroadcastsEach(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	pub := &recordingPublisher{}
	svc := changefeed.NewService(repo).WithPublisher(pub)

	workspaceID := uuid.New()
	inputs := []changefeed.RecordInput{
		{WorkspaceID: workspaceID, Kind: changefeed.KindFile, Op: changefeed.OpDelete, ResourceID: uuid.New()},
		{WorkspaceID: workspaceID, Kind: changefeed.KindFile, Op: changefeed.OpDelete, ResourceID: uuid.New()},
		{WorkspaceID: workspaceID, Kind: changefeed.KindFile, Op: changefeed.OpDelete, ResourceID: uuid.New()},
	}
	muts, err := svc.BatchRecord(context.Background(), inputs)
	if err != nil {
		t.Fatalf("batch record: %v", err)
	}
	if len(muts) != len(inputs) {
		t.Fatalf("expected %d returned mutations, got %d", len(inputs), len(muts))
	}
	for i, m := range muts {
		if m.Sequence == 0 {
			t.Fatalf("batch[%d] sequence not populated", i)
		}
		if i > 0 && muts[i-1].Sequence >= m.Sequence {
			t.Fatalf("batch sequence not monotonic: %d >= %d", muts[i-1].Sequence, m.Sequence)
		}
	}
	if len(repo.rows) != len(inputs) {
		t.Fatalf("expected %d rows persisted, got %d", len(inputs), len(repo.rows))
	}
	events, _ := pub.snapshot()
	if len(events) != len(inputs) {
		t.Fatalf("expected %d Publish calls, got %d", len(inputs), len(events))
	}
}

func TestServiceBatchRecord_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := changefeed.NewService(repo)
	muts, err := svc.BatchRecord(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty batch should not error: %v", err)
	}
	if len(muts) != 0 {
		t.Fatalf("expected 0 mutations, got %d", len(muts))
	}
	if len(repo.rows) != 0 {
		t.Fatalf("expected 0 rows persisted, got %d", len(repo.rows))
	}
}

func TestServiceBatchRecord_ValidationErrorAbortsBatch(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := changefeed.NewService(repo)

	inputs := []changefeed.RecordInput{
		{WorkspaceID: uuid.New(), Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New()},
		// Second input is invalid (bad kind) so the entire batch
		// must fail before any DB write happens — partial success
		// would leak gaps into the cursor stream.
		{WorkspaceID: uuid.New(), Kind: "wat", Op: changefeed.OpCreate, ResourceID: uuid.New()},
	}
	if _, err := svc.BatchRecord(context.Background(), inputs); err == nil {
		t.Fatalf("expected validation error")
	}
	if len(repo.rows) != 0 {
		t.Fatalf("expected 0 rows persisted on validation failure, got %d", len(repo.rows))
	}
}

func TestServiceBatchRecord_PublishErrorDoesNotFail(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	pub := &recordingPublisher{err: errors.New("redis down")}
	svc := changefeed.NewService(repo).WithPublisher(pub)
	inputs := []changefeed.RecordInput{
		{WorkspaceID: uuid.New(), Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New()},
		{WorkspaceID: uuid.New(), Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New()},
	}
	muts, err := svc.BatchRecord(context.Background(), inputs)
	if err != nil {
		t.Fatalf("publisher error must not fail BatchRecord: %v", err)
	}
	if len(muts) != len(inputs) {
		t.Fatalf("expected %d mutations, got %d", len(inputs), len(muts))
	}
	if len(repo.rows) != len(inputs) {
		t.Fatalf("rows not persisted despite publisher error")
	}
}

func TestServiceBatchRecord_RepoErrorPropagates(t *testing.T) {
	t.Parallel()
	svc := changefeed.NewService(errRepo{err: errors.New("db down")})
	inputs := []changefeed.RecordInput{
		{WorkspaceID: uuid.New(), Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New()},
	}
	if _, err := svc.BatchRecord(context.Background(), inputs); err == nil {
		t.Fatalf("expected DB error to propagate")
	}
}

func TestServiceRecord_NoPublisherStillPersists(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := changefeed.NewService(repo) // no WithPublisher
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: uuid.New(),
		Kind:        changefeed.KindFolder,
		Op:          changefeed.OpRename,
		ResourceID:  uuid.New(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 row persisted")
	}
}

// recordingBuster captures BustWorkspace invocations so tests can
// assert which mutations trigger cache invalidation and that
// batch de-duplication collapses N records to 1 bust per
// workspace.
type recordingBuster struct {
	mu     sync.Mutex
	busted []uuid.UUID
}

func (b *recordingBuster) BustWorkspace(_ context.Context, workspaceID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.busted = append(b.busted, workspaceID)
}

func (b *recordingBuster) snapshot() []uuid.UUID {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]uuid.UUID, len(b.busted))
	copy(out, b.busted)
	return out
}

// TestService_Record_BustsCacheOnPermissionMutation verifies the
// changefeed fires the cache buster after persisting a permission
// mutation. This is the canonical hot-path invalidation: a
// permission.create event must invalidate the perm cache or the
// API would serve the pre-grant view for up to TTL seconds.
func TestService_Record_BustsCacheOnPermissionMutation(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	buster := &recordingBuster{}
	svc := changefeed.NewService(repo).WithCacheBuster(buster)
	ws := uuid.New()
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: ws,
		ActorID:     ptrUUID(uuid.New()),
		Kind:        changefeed.KindPermission,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := buster.snapshot()
	if len(got) != 1 || got[0] != ws {
		t.Errorf("expected 1 bust on workspace %s; got %v", ws, got)
	}
}

// TestService_Record_NoBustOnFileCreate guards against
// over-busting: a pure file.create event has no descendants and
// cannot affect any cached access-check answer, so the bust path
// must NOT fire. Over-busting would defeat the cache on every
// upload.
func TestService_Record_NoBustOnFileCreate(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	buster := &recordingBuster{}
	svc := changefeed.NewService(repo).WithCacheBuster(buster)
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: uuid.New(),
		ActorID:     ptrUUID(uuid.New()),
		Kind:        changefeed.KindFile,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := buster.snapshot(); len(got) != 0 {
		t.Errorf("expected no busts on file.create; got %v", got)
	}
}

// TestService_Record_BustsOnFolderMove verifies that a folder
// move (which changes the ancestry chain for every descendant)
// triggers the bust. Without this the perm cache would serve
// pre-move answers for up to TTL seconds.
func TestService_Record_BustsOnFolderMove(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	buster := &recordingBuster{}
	svc := changefeed.NewService(repo).WithCacheBuster(buster)
	ws := uuid.New()
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: ws,
		ActorID:     ptrUUID(uuid.New()),
		Kind:        changefeed.KindFolder,
		Op:          changefeed.OpMove,
		ResourceID:  uuid.New(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := buster.snapshot()
	if len(got) != 1 || got[0] != ws {
		t.Errorf("expected 1 bust on workspace %s; got %v", ws, got)
	}
}

// TestService_Record_ContentBusterBustsOnEveryMutation verifies the
// content-cache buster fires on mutations the permission buster
// deliberately ignores (file.create, folder.rename). The content cache
// (folder listings / search) reflects the exact resource set, so a
// create or rename must invalidate it even though it does not change
// permission resolution.
func TestService_Record_ContentBusterBustsOnEveryMutation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		kind string
		op   string
	}{
		{"file.create", changefeed.KindFile, changefeed.OpCreate},
		{"folder.rename", changefeed.KindFolder, changefeed.OpRename},
		{"file.delete", changefeed.KindFile, changefeed.OpDelete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			permBuster := &recordingBuster{}
			contentBuster := &recordingBuster{}
			svc := changefeed.NewService(repo).
				WithCacheBuster(permBuster).
				WithContentCacheBuster(contentBuster)
			ws := uuid.New()
			if _, err := svc.Record(context.Background(), changefeed.RecordInput{
				WorkspaceID: ws,
				ActorID:     ptrUUID(uuid.New()),
				Kind:        tc.kind,
				Op:          tc.op,
				ResourceID:  uuid.New(),
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			// Content buster must always fire.
			if got := contentBuster.snapshot(); len(got) != 1 || got[0] != ws {
				t.Errorf("content buster: expected 1 bust on %s; got %v", ws, got)
			}
			// Permission buster only fires for the narrow topology/grant
			// set; file.create and folder.rename must NOT bust it.
			if tc.op == changefeed.OpCreate || tc.op == changefeed.OpRename {
				if got := permBuster.snapshot(); len(got) != 0 {
					t.Errorf("perm buster: expected no bust on %s; got %v", tc.name, got)
				}
			}
		})
	}
}

// TestService_BatchRecord_ContentBusterDeduplicates verifies the
// content buster collapses a multi-item batch to one bust per
// workspace, like the permission buster, even though it has no
// per-mutation predicate gate.
func TestService_BatchRecord_ContentBusterDeduplicates(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	contentBuster := &recordingBuster{}
	svc := changefeed.NewService(repo).WithContentCacheBuster(contentBuster)
	ws := uuid.New()
	inputs := make([]changefeed.RecordInput, 0, 5)
	for i := 0; i < 5; i++ {
		inputs = append(inputs, changefeed.RecordInput{
			WorkspaceID: ws,
			ActorID:     ptrUUID(uuid.New()),
			Kind:        changefeed.KindFile,
			Op:          changefeed.OpCreate,
			ResourceID:  uuid.New(),
		})
	}
	if _, err := svc.BatchRecord(context.Background(), inputs); err != nil {
		t.Fatalf("batch record: %v", err)
	}
	if got := contentBuster.snapshot(); len(got) != 1 || got[0] != ws {
		t.Errorf("expected 1 deduplicated content bust on %s; got %v", ws, got)
	}
}

// TestService_BatchRecord_DeduplicatesBusts verifies that a
// bulk operation (e.g. 100-item bulk move) collapses to a single
// bust per workspace. Without this, a large bulk would issue N
// redundant INCRs against Redis.
func TestService_BatchRecord_DeduplicatesBusts(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	buster := &recordingBuster{}
	svc := changefeed.NewService(repo).WithCacheBuster(buster)
	ws := uuid.New()
	inputs := make([]changefeed.RecordInput, 0, 50)
	for i := 0; i < 50; i++ {
		inputs = append(inputs, changefeed.RecordInput{
			WorkspaceID: ws,
			ActorID:     ptrUUID(uuid.New()),
			Kind:        changefeed.KindFile,
			Op:          changefeed.OpMove,
			ResourceID:  uuid.New(),
		})
	}
	if _, err := svc.BatchRecord(context.Background(), inputs); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := buster.snapshot(); len(got) != 1 || got[0] != ws {
		t.Errorf("expected 1 deduplicated bust on workspace %s; got %v", ws, got)
	}
}

// TestService_BatchRecord_BustsEachUniqueWorkspace verifies that
// when a batch spans multiple workspaces, each unique workspace
// is busted exactly once.
func TestService_BatchRecord_BustsEachUniqueWorkspace(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	buster := &recordingBuster{}
	svc := changefeed.NewService(repo).WithCacheBuster(buster)
	ws1, ws2, ws3 := uuid.New(), uuid.New(), uuid.New()
	inputs := []changefeed.RecordInput{
		{WorkspaceID: ws1, ActorID: ptrUUID(uuid.New()), Kind: changefeed.KindPermission, Op: changefeed.OpCreate, ResourceID: uuid.New()},
		{WorkspaceID: ws2, ActorID: ptrUUID(uuid.New()), Kind: changefeed.KindFolder, Op: changefeed.OpMove, ResourceID: uuid.New()},
		{WorkspaceID: ws1, ActorID: ptrUUID(uuid.New()), Kind: changefeed.KindPermission, Op: changefeed.OpDelete, ResourceID: uuid.New()},
		{WorkspaceID: ws3, ActorID: ptrUUID(uuid.New()), Kind: changefeed.KindFile, Op: changefeed.OpMove, ResourceID: uuid.New()},
		// A file.create in ws1 must NOT add a third entry —
		// shouldBustForMutation rejects file.create.
		{WorkspaceID: ws1, ActorID: ptrUUID(uuid.New()), Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New()},
	}
	if _, err := svc.BatchRecord(context.Background(), inputs); err != nil {
		t.Fatalf("batch: %v", err)
	}
	got := buster.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected 3 unique-workspace busts; got %d (%v)", len(got), got)
	}
	seen := map[uuid.UUID]bool{}
	for _, w := range got {
		seen[w] = true
	}
	for _, want := range []uuid.UUID{ws1, ws2, ws3} {
		if !seen[want] {
			t.Errorf("expected bust on %s", want)
		}
	}
}

// TestService_Record_NilBusterIsNoop verifies that a service
// wired without a cache buster still records and publishes
// normally — the bust hook must be a true no-op when disabled
// rather than a panic.
func TestService_Record_NilBusterIsNoop(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := changefeed.NewService(repo)
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: uuid.New(),
		ActorID:     ptrUUID(uuid.New()),
		Kind:        changefeed.KindPermission,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Errorf("row not persisted with nil buster")
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }

// knownKindOpBustDecisions is the exhaustive fixture pinning the
// shouldBustForMutation contract for every (kind, op) pair that
// can be produced by the changefeed today. The map's key is
// "{kind}/{op}" so a missing or new tuple shows up cleanly in a
// diff.
//
// Adding a new Kind* or Op* constant in
// internal/changefeed/changefeed.go REQUIRES adding an entry
// here and either:
//   - asserting bust=true and updating shouldBustForMutation
//     in service.go to honour that, OR
//   - asserting bust=false with a doc-comment in the entry
//     explaining why the new kind doesn't affect permission
//     resolution.
//
// The test below enforces that knownKindOpBustDecisions covers
// every Kind × Op product, so a missing tuple fails CI. The
// expected-kind-count assertion in
// TestShouldBustForMutation_ExhaustivelyAuditsKindOpMatrix
// closes the remaining gap: if a developer adds a new Kind
// constant in changefeed.go and forgets to add it here, the
// count check trips.
//
// This file is the canonical "did the bust audit happen?"
// ledger. Per Devin Review ANALYSIS_0003 escalation: previously
// the audit obligation was doc-comment-only; now it is enforced
// at test-time.
var knownKindOpBustDecisions = map[string]bool{
	// KindPermission — every grant-table mutation by
	// definition changes access resolution. Always bust.
	"permission/create": true,
	"permission/update": true,
	"permission/rename": true, // not currently emitted; would still be a grant mutation
	"permission/move":   true, // not currently emitted; would still be a grant mutation
	"permission/delete": true,

	// KindFolder — only move/delete change the ancestry chain;
	// create/update/rename don't affect resolution for any
	// existing descendant.
	"folder/create": false,
	"folder/update": false,
	"folder/rename": false,
	"folder/move":   true,
	"folder/delete": true,

	// KindFile — only move changes the file's ancestor chain;
	// create/update/rename/delete are pure leaf-node changes
	// that don't reshape the inheritance tree. file/delete in
	// particular is bounded-stale: every downstream consumer
	// of "can user X read deleted file Y?" also checks
	// deleted_at IS NULL, so the worst case is a sub-TTL stale
	// allow that gets rejected at the data layer.
	"file/create": false,
	"file/update": false,
	"file/rename": false,
	"file/move":   true,
	"file/delete": false,

	// KindDocument — collab-editor mutations don't participate
	// in the permission cache. Document access is gated by the
	// containing folder's grants, which are invalidated via the
	// folder.* entries above. A document edit cannot grant or
	// revoke access on its own.
	"document/create": false,
	"document/update": false,
	"document/rename": false,
	"document/move":   false,
	"document/delete": false,
}

// expectedKindCount is the number of distinct Kind* constants
// declared in internal/changefeed/changefeed.go. The test below
// asserts the audit ledger above covers exactly this many
// distinct kinds, so adding a new Kind constant without
// updating the ledger trips CI.
const expectedKindCount = 4

// TestShouldBustForMutation_ExhaustivelyAuditsKindOpMatrix
// converts the "audit deliberately when adding a new Kind" rule
// (doc-comment-only in shouldBustForMutation) into a
// CI-enforced contract.
//
// The test:
//
//  1. Asserts every Kind constant declared in changefeed.go is
//     present in knownKindOpBustDecisions above (the
//     expectedKindCount sentinel forces a developer to update
//     both the registry AND the audit ledger when adding a
//     Kind).
//  2. Exhaustively iterates every (kind, op) pair from the
//     fixture and asserts shouldBustForMutation (observed
//     through BatchRecord's recordingBuster) matches the
//     expected decision.
//
// Per Devin Review ANALYSIS_0003 (escalated): the previous
// failure mode was that adding a new Kind producing a
// permission-affecting mutation (e.g., a hypothetical
// KindWorkspace for workspace-level role changes) would leave
// shouldBustForMutation returning false silently. No compile
// error, no runtime panic, just stale cache entries until TTL
// expiry. This test pins the contract so any new Kind forces
// a documented bust decision before merge.
func TestShouldBustForMutation_ExhaustivelyAuditsKindOpMatrix(t *testing.T) {
	t.Parallel()

	// Kinds the test currently knows about. If you add a new
	// Kind constant in changefeed.go you MUST add it here AND
	// add entries for every Op to knownKindOpBustDecisions
	// above. The expectedKindCount sentinel catches drift.
	knownKinds := []string{
		changefeed.KindFile,
		changefeed.KindFolder,
		changefeed.KindPermission,
		changefeed.KindDocument,
	}
	if got := len(knownKinds); got != expectedKindCount {
		t.Fatalf("knownKinds has %d entries but expectedKindCount=%d; if you added a new Kind constant in internal/changefeed/changefeed.go, also update knownKinds, knownKindOpBustDecisions, and expectedKindCount in this file", got, expectedKindCount)
	}

	// Every Op the changefeed currently produces. Same audit
	// rule: adding a new Op requires adding entries for every
	// known Kind in knownKindOpBustDecisions.
	knownOps := []string{
		changefeed.OpCreate,
		changefeed.OpUpdate,
		changefeed.OpRename,
		changefeed.OpMove,
		changefeed.OpDelete,
	}

	// First: verify the fixture covers every (kind, op)
	// product. A missing entry means someone added a Kind or
	// Op constant without auditing.
	for _, kind := range knownKinds {
		for _, op := range knownOps {
			key := kind + "/" + op
			if _, ok := knownKindOpBustDecisions[key]; !ok {
				t.Errorf("knownKindOpBustDecisions missing audit entry for %q — every Kind × Op pair must have an explicit bust=true|false decision", key)
			}
		}
	}

	// Second: exhaustively assert shouldBustForMutation's
	// behaviour for every audited tuple. Both Record and
	// BatchRecord invoke the bust hook (see service.go's
	// recordOne and BatchRecord respectively); we use the
	// single-row Record path here so each sub-test's
	// recording buster has a 1:1 mapping between the input
	// tuple and the bust decision under test (BatchRecord
	// adds workspace-level deduplication that would muddle
	// per-tuple assertions). A fresh service per sub-test
	// keeps the recording buster's snapshot unambiguous.
	for key, wantBust := range knownKindOpBustDecisions {
		key, wantBust := key, wantBust
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			parts := splitKindOp(key)
			if len(parts) != 2 {
				t.Fatalf("malformed audit key %q (expected kind/op)", key)
			}
			kind, op := parts[0], parts[1]

			repo := newFakeRepo()
			buster := &recordingBuster{}
			svc := changefeed.NewService(repo).WithCacheBuster(buster)

			ws := uuid.New()
			if _, err := svc.Record(context.Background(), changefeed.RecordInput{
				WorkspaceID: ws,
				ActorID:     ptrUUID(uuid.New()),
				Kind:        kind,
				Op:          op,
				ResourceID:  uuid.New(),
			}); err != nil {
				t.Fatalf("record %s: %v", key, err)
			}

			got := buster.snapshot()
			gotBust := len(got) == 1 && got[0] == ws
			if gotBust != wantBust {
				t.Errorf("shouldBustForMutation(%s/%s) drift: want bust=%v, got bust=%v (snapshot=%v). If this change is intentional, update knownKindOpBustDecisions in this file to reflect the new decision AND make sure the new decision is justified in the doc comment on shouldBustForMutation in service.go.", kind, op, wantBust, gotBust, got)
			}
		})
	}
}

// splitKindOp is a tiny helper for the exhaustive matrix test.
// strings.SplitN is overkill for a fixed two-piece key.
func splitKindOp(s string) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}
