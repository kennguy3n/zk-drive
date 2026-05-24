package drive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/changefeed"
)

// fakeChangefeedRepo is the in-process Repository used by handler
// tests to exercise the REST endpoints without Postgres. It mirrors
// the test fake in the changefeed package's own _test files.
type fakeChangefeedRepo struct {
	mu   sync.Mutex
	next int64
	rows []changefeed.Mutation
}

func (r *fakeChangefeedRepo) Record(_ context.Context, m *changefeed.Mutation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	m.Sequence = r.next
	m.OccurredAt = time.Now().UTC()
	r.rows = append(r.rows, *m)
	return nil
}

func (r *fakeChangefeedRepo) Since(_ context.Context, workspaceID uuid.UUID, cursor int64, limit int) ([]changefeed.Mutation, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matched []changefeed.Mutation
	for _, row := range r.rows {
		if row.WorkspaceID != workspaceID || row.Sequence <= cursor {
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

func (r *fakeChangefeedRepo) Latest(_ context.Context, workspaceID uuid.UUID) (int64, error) {
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

// authenticated returns a request whose context already carries the
// workspace + user ids that the production middleware would have
// attached. Avoids spinning up the full chi router for unit tests of
// the change endpoints.
func authenticated(t *testing.T, method, path string, workspaceID, userID uuid.UUID) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	ctx := middleware.WithWorkspaceID(req.Context(), workspaceID)
	ctx = middleware.WithUserID(ctx, userID)
	return req.WithContext(ctx)
}

func TestListChanges_ReturnsRowsForWorkspace(t *testing.T) {
	t.Parallel()

	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)

	wsID := uuid.New()
	other := uuid.New()
	resID := uuid.New()
	// Two mutations in the caller's workspace, one in another
	// workspace which must not leak into the response.
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: wsID, Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: resID,
	}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: other, Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New(),
	}); err != nil {
		t.Fatalf("record other: %v", err)
	}
	if _, err := svc.Record(context.Background(), changefeed.RecordInput{
		WorkspaceID: wsID, Kind: changefeed.KindFolder, Op: changefeed.OpRename, ResourceID: uuid.New(),
	}); err != nil {
		t.Fatalf("record 3: %v", err)
	}

	req := authenticated(t, http.MethodGet, "/api/changes", wsID, uuid.New())
	w := httptest.NewRecorder()
	h.ListChanges(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp changesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Mutations) != 2 {
		t.Fatalf("mutations len = %d, want 2; payload=%s", len(resp.Mutations), w.Body.String())
	}
	for _, m := range resp.Mutations {
		if m.WorkspaceID != wsID {
			t.Fatalf("leaked workspace %s into response", m.WorkspaceID)
		}
	}
	if resp.Cursor == 0 {
		t.Fatalf("cursor was not advanced")
	}
	if resp.HasMore {
		t.Fatalf("has_more should be false with 2 rows under default limit")
	}
}

func TestListChanges_SinceCursorAdvancesPaging(t *testing.T) {
	t.Parallel()

	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)
	wsID := uuid.New()

	for i := 0; i < 4; i++ {
		if _, err := svc.Record(context.Background(), changefeed.RecordInput{
			WorkspaceID: wsID, Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New(),
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	// First page (limit=2)
	req := authenticated(t, http.MethodGet, "/api/changes?limit=2", wsID, uuid.New())
	w := httptest.NewRecorder()
	h.ListChanges(w, req)
	var page1 changesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1.Mutations) != 2 || !page1.HasMore {
		t.Fatalf("page1 = %+v", page1)
	}

	// Second page using the advanced cursor
	req2 := authenticated(t, http.MethodGet,
		"/api/changes?limit=10&since="+itoa(page1.Cursor), wsID, uuid.New())
	w2 := httptest.NewRecorder()
	h.ListChanges(w2, req2)
	var page2 changesResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &page2)
	if len(page2.Mutations) != 2 || page2.HasMore {
		t.Fatalf("page2 = %+v", page2)
	}
}

func TestListChanges_RejectsInvalidQuery(t *testing.T) {
	t.Parallel()
	svc := changefeed.NewService(&fakeChangefeedRepo{})
	h := (&Handler{}).WithChangefeed(svc)
	cases := []string{
		"/api/changes?since=not-a-number",
		"/api/changes?limit=banana",
	}
	for _, path := range cases {
		req := authenticated(t, http.MethodGet, path, uuid.New(), uuid.New())
		w := httptest.NewRecorder()
		h.ListChanges(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", path, w.Code)
		}
	}
}

func TestListChanges_NotConfigured(t *testing.T) {
	t.Parallel()
	h := &Handler{} // no WithChangefeed
	req := authenticated(t, http.MethodGet, "/api/changes", uuid.New(), uuid.New())
	w := httptest.NewRecorder()
	h.ListChanges(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

func TestListChanges_Unauthenticated(t *testing.T) {
	t.Parallel()
	svc := changefeed.NewService(&fakeChangefeedRepo{})
	h := (&Handler{}).WithChangefeed(svc)
	req := httptest.NewRequest(http.MethodGet, "/api/changes", nil)
	w := httptest.NewRecorder()
	h.ListChanges(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestLatestChange(t *testing.T) {
	t.Parallel()

	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)
	wsID := uuid.New()

	// Empty workspace → 0.
	req := authenticated(t, http.MethodGet, "/api/changes/latest", wsID, uuid.New())
	w := httptest.NewRecorder()
	h.LatestChange(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Cursor int64 `json:"cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Cursor != 0 {
		t.Fatalf("empty latest = %d, want 0", body.Cursor)
	}

	// Record three; latest should be 3.
	for i := 0; i < 3; i++ {
		if _, err := svc.Record(context.Background(), changefeed.RecordInput{
			WorkspaceID: wsID, Kind: changefeed.KindFile, Op: changefeed.OpCreate, ResourceID: uuid.New(),
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	w2 := httptest.NewRecorder()
	h.LatestChange(w2, authenticated(t, http.MethodGet, "/api/changes/latest", wsID, uuid.New()))
	_ = json.Unmarshal(w2.Body.Bytes(), &body)
	if body.Cursor != 3 {
		t.Fatalf("latest = %d, want 3", body.Cursor)
	}
}

// TestChangefeedKindOpFor pins the activity → change-feed action map
// so a future activity action constant addition does not silently
// fall off the change feed.
func TestChangefeedKindOpFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		action       string
		expectedKind string
		expectedOp   string
		shouldEmit   bool
	}{
		{activity.ActionFileCreate, changefeed.KindFile, changefeed.OpCreate, true},
		{activity.ActionFileUpload, changefeed.KindFile, changefeed.OpUpdate, true},
		{activity.ActionFileRename, changefeed.KindFile, changefeed.OpRename, true},
		{activity.ActionFileMove, changefeed.KindFile, changefeed.OpMove, true},
		{activity.ActionFileDelete, changefeed.KindFile, changefeed.OpDelete, true},
		{activity.ActionFileBulkMove, changefeed.KindFile, changefeed.OpMove, true},
		{activity.ActionFileBulkDelete, changefeed.KindFile, changefeed.OpDelete, true},
		{activity.ActionFileBulkCopy, changefeed.KindFile, changefeed.OpCreate, true},
		{activity.ActionFileTagAdd, changefeed.KindFile, changefeed.OpUpdate, true},
		{activity.ActionFileTagRemove, changefeed.KindFile, changefeed.OpUpdate, true},
		{activity.ActionFolderCreate, changefeed.KindFolder, changefeed.OpCreate, true},
		{activity.ActionFolderRename, changefeed.KindFolder, changefeed.OpRename, true},
		{activity.ActionFolderMove, changefeed.KindFolder, changefeed.OpMove, true},
		{activity.ActionFolderDelete, changefeed.KindFolder, changefeed.OpDelete, true},
		{activity.ActionPermGrant, changefeed.KindPermission, changefeed.OpCreate, true},
		{activity.ActionPermRevoke, changefeed.KindPermission, changefeed.OpDelete, true},
		// Read events explicitly absent:
		{activity.ActionFileDownload, "", "", false},
		{activity.ActionFileBulkDownload, "", "", false},
		{"made-up.action", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			gotKind, gotOp, ok := changefeedKindOpFor(tc.action, "file")
			if ok != tc.shouldEmit {
				t.Fatalf("shouldEmit=%v, got %v", tc.shouldEmit, ok)
			}
			if gotKind != tc.expectedKind {
				t.Fatalf("kind = %q, want %q", gotKind, tc.expectedKind)
			}
			if gotOp != tc.expectedOp {
				t.Fatalf("op = %q, want %q", gotOp, tc.expectedOp)
			}
		})
	}
}

func TestUUIDFromAny(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	cases := []struct {
		name string
		in   any
		ok   bool
	}{
		{"uuid.UUID", id, true},
		{"*uuid.UUID", &id, true},
		{"string", id.String(), true},
		{"empty string", "", false},
		{"bogus string", "not-a-uuid", false},
		{"nil pointer", (*uuid.UUID)(nil), false},
		{"int", 42, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := uuidFromAny(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != id {
				t.Fatalf("uuid mismatch: %s vs %s", got, id)
			}
		})
	}
}

func TestRecordChange_PullsParentAndNameFromMetadata(t *testing.T) {
	t.Parallel()

	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{
		activity: nil, // logActivity short-circuits when activity is nil
	}).WithChangefeed(svc)

	wsID := uuid.New()
	userID := uuid.New()
	resID := uuid.New()
	parentID := uuid.New()

	ctx := middleware.WithWorkspaceID(context.Background(), wsID)
	ctx = middleware.WithUserID(ctx, userID)

	h.recordChange(ctx, wsID, userID, activity.ActionFileCreate, "file", resID, map[string]any{
		"folder_id": parentID.String(),
		"name":      "report.pdf",
	})
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 mutation recorded, got %d", len(repo.rows))
	}
	m := repo.rows[0]
	if m.ParentID == nil || *m.ParentID != parentID {
		t.Fatalf("parent_id not lifted from metadata: %+v", m.ParentID)
	}
	if m.Name != "report.pdf" {
		t.Fatalf("name = %q, want report.pdf", m.Name)
	}
	if m.ActorID == nil || *m.ActorID != userID {
		t.Fatalf("actor_id not set: %+v", m.ActorID)
	}
}

func TestRecordChange_SkipsNonMutationActions(t *testing.T) {
	t.Parallel()
	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)
	h.recordChange(context.Background(), uuid.New(), uuid.New(),
		activity.ActionFileDownload, "file", uuid.New(), nil)
	if len(repo.rows) != 0 {
		t.Fatalf("download produced a change row; rows=%+v", repo.rows)
	}
}

func TestRecordChange_NoChangefeedServiceIsNoop(t *testing.T) {
	t.Parallel()
	h := &Handler{} // changefeed nil
	// Must not panic.
	h.recordChange(context.Background(), uuid.New(), uuid.New(),
		activity.ActionFileCreate, "file", uuid.New(), nil)
}

// itoa avoids importing strconv just for one int formatting call.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
