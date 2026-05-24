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

func (e errRepo) Record(_ context.Context, _ *changefeed.Mutation) error { return e.err }
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
