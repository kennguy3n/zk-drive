package drive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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

func (r *fakeChangefeedRepo) BatchRecord(_ context.Context, muts []changefeed.Mutation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range muts {
		r.next++
		muts[i].Sequence = r.next
		muts[i].OccurredAt = time.Now().UTC()
		r.rows = append(r.rows, muts[i])
	}
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

// TestLogActivity_RecordsChangeEvenWhenActivityNil pins the F1 fix:
// the change-feed leg must remain wired even if no activity service
// is configured. Before the decoupling, logActivity returned early
// on activity == nil and silently disabled the change feed.
func TestLogActivity_RecordsChangeEvenWhenActivityNil(t *testing.T) {
	t.Parallel()
	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc) // activity intentionally nil

	wsID := uuid.New()
	userID := uuid.New()
	resID := uuid.New()
	ctx := middleware.WithWorkspaceID(context.Background(), wsID)
	ctx = middleware.WithUserID(ctx, userID)

	h.logActivity(ctx, activity.ActionFileCreate, "file", resID, nil)
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 change row recorded when activity is nil, got %d", len(repo.rows))
	}
}

// TestBuildChangefeedInput_StripsLiftedKeysFromMetadata pins the F3
// fix: parent_id / name extracted into structured columns must NOT
// also appear in change_log.metadata, which would double the row
// size for the most common mutation actions.
func TestBuildChangefeedInput_StripsLiftedKeysFromMetadata(t *testing.T) {
	t.Parallel()
	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)

	wsID := uuid.New()
	userID := uuid.New()
	resID := uuid.New()
	parentID := uuid.New()
	h.recordChange(context.Background(), wsID, userID,
		activity.ActionFileCreate, "file", resID, map[string]any{
			"folder_id":            parentID.String(),
			"parent_folder_id":     parentID.String(),
			"new_parent_folder_id": parentID.String(),
			"name":                 "report.pdf",
			"size_bytes":           int64(2048), // unrelated metadata stays
		})
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 mutation recorded, got %d", len(repo.rows))
	}
	m := repo.rows[0]
	if m.ParentID == nil || *m.ParentID != parentID {
		t.Fatalf("parent_id not lifted: %+v", m.ParentID)
	}
	if m.Name != "report.pdf" {
		t.Fatalf("name not lifted: %q", m.Name)
	}
	// The metadata JSON should retain size_bytes but not the
	// lifted keys.
	body := string(m.Metadata)
	for _, k := range []string{"folder_id", "parent_folder_id", "new_parent_folder_id", "\"name\""} {
		if strings.Contains(body, k) {
			t.Fatalf("metadata still contains lifted key %q: %s", k, body)
		}
	}
	if !strings.Contains(body, "size_bytes") {
		t.Fatalf("metadata missing unrelated key size_bytes: %s", body)
	}
}

// TestBuildChangefeedInput_NoMetadataLeftEmpty drops the metadata
// JSON entirely when stripping leaves an empty object — a 2-byte
// "{}" payload per row would inflate the table for no reason.
func TestBuildChangefeedInput_NoMetadataLeftEmpty(t *testing.T) {
	t.Parallel()
	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)

	parentID := uuid.New()
	h.recordChange(context.Background(), uuid.New(), uuid.New(),
		activity.ActionFileCreate, "file", uuid.New(), map[string]any{
			"folder_id": parentID.String(),
			"name":      "x.txt",
		})
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(repo.rows))
	}
	if len(repo.rows[0].Metadata) != 0 {
		t.Fatalf("metadata should be empty after stripping all lifted keys, got %q", repo.rows[0].Metadata)
	}
}

// TestChangefeedKindOpFor_ExhaustiveOverAllActions enforces that
// every activity.Action* constant is either mapped to a change-feed
// (kind, op) pair OR explicitly listed in changefeedReadOnlyActions.
// Adding a new constant without updating one of the two will fail
// this test instead of silently disappearing from the sync stream.
func TestChangefeedKindOpFor_ExhaustiveOverAllActions(t *testing.T) {
	t.Parallel()
	for _, action := range activity.AllActions {
		_, _, mapped := changefeedKindOpFor(action, "")
		_, skipped := changefeedReadOnlyActions[action]
		if !mapped && !skipped {
			t.Fatalf("activity action %q is neither mapped in changefeedKindOpFor "+
				"nor listed in changefeedReadOnlyActions — sync clients will silently "+
				"miss this mutation. Add to one of the two before merging.", action)
		}
		if mapped && skipped {
			t.Fatalf("activity action %q is BOTH mapped and read-only — pick one.", action)
		}
	}
}

// TestBuildChangefeedInput_LiftsTargetForBulkMove pins the F8 fix:
// BulkMove's legacy "target" metadata key is recognised as a parent
// key (lifted into ParentID and stripped from the JSONB blob), so a
// bulk move no longer carries a duplicate "target": UUID alongside
// the structured parent_id column.
func TestBuildChangefeedInput_LiftsTargetForBulkMove(t *testing.T) {
	t.Parallel()
	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)
	targetID := uuid.New()
	ctx := middleware.WithWorkspaceID(context.Background(), uuid.New())
	ctx = middleware.WithUserID(ctx, uuid.New())
	h.batchRecordChanges(ctx, []changeInput{{
		Action: activity.ActionFileBulkMove, ResourceType: "file",
		ResourceID: uuid.New(),
		Metadata:   map[string]any{"target": targetID},
	}})
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 batch row, got %d", len(repo.rows))
	}
	m := repo.rows[0]
	if m.ParentID == nil || *m.ParentID != targetID {
		t.Fatalf("target not lifted to ParentID: %+v", m.ParentID)
	}
	if strings.Contains(string(m.Metadata), "target") {
		t.Fatalf("target not stripped from metadata: %s", m.Metadata)
	}
}

// TestBuildChangefeedInput_StripsTypedNilParentKey pins the F9 fix:
// a present-but-unlifted parent key (e.g. root folder create with a
// typed-nil *uuid.UUID) must still be stripped from the metadata
// JSONB so the wire format is consistent between root and non-root
// creates. The structured parent_id column is null in both cases.
func TestBuildChangefeedInput_StripsTypedNilParentKey(t *testing.T) {
	t.Parallel()
	repo := &fakeChangefeedRepo{}
	svc := changefeed.NewService(repo)
	h := (&Handler{}).WithChangefeed(svc)
	var nilParent *uuid.UUID // root folder
	ctx := middleware.WithWorkspaceID(context.Background(), uuid.New())
	ctx = middleware.WithUserID(ctx, uuid.New())
	h.recordChange(ctx, uuid.New(), uuid.New(),
		activity.ActionFolderCreate, "folder", uuid.New(),
		map[string]any{
			"parent_folder_id": nilParent,
			"name":             "Root",
			"some_other":       "kept",
		})
	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(repo.rows))
	}
	m := repo.rows[0]
	if m.ParentID != nil {
		t.Fatalf("structured ParentID should be nil for root create, got %v", m.ParentID)
	}
	body := string(m.Metadata)
	if strings.Contains(body, "parent_folder_id") {
		t.Fatalf("parent_folder_id should be stripped on typed-nil too: %s", body)
	}
	if strings.Contains(body, "\"name\"") {
		t.Fatalf("name should be stripped: %s", body)
	}
	if !strings.Contains(body, "some_other") {
		t.Fatalf("unrelated metadata key dropped: %s", body)
	}
}

// TestParseIntQuery_NegativeClipsToDefault pins the F5 symmetry fix:
// a negative `limit` should be treated identically to an unset
// `limit`, matching parseInt64Query's negative-clipping behavior.
func TestParseIntQuery_NegativeClipsToDefault(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api/changes?limit=-5", nil)
	v, err := parseIntQuery(req, "limit", 100)
	if err != nil {
		t.Fatalf("parseIntQuery returned error: %v", err)
	}
	if v != 100 {
		t.Fatalf("negative limit should clip to default 100, got %d", v)
	}
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
