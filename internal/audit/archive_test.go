package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeArchiveRepo is an in-memory ArchiveRepository for unit tests.
// It records every mutation (fetch cursor, deletes, run records) so
// tests can assert idempotency and ordering invariants without a
// live Postgres dependency.
type fakeArchiveRepo struct {
	mu       sync.Mutex
	rows     []*Entry              // ordered by id ASC
	runs     []*ArchiveRunRecord   // append-only insertion order
	deleted  map[uuid.UUID]bool    // ids the archiver asked to delete
	failures map[string]error      // method -> error to return next time
}

func newFakeRepo(rows []*Entry) *fakeArchiveRepo {
	return &fakeArchiveRepo{rows: rows, deleted: make(map[uuid.UUID]bool), failures: make(map[string]error)}
}

func (f *fakeArchiveRepo) injectFailure(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = err
}

func (f *fakeArchiveRepo) consumeFailure(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err, ok := f.failures[method]
	if !ok {
		return nil
	}
	delete(f.failures, method)
	return err
}

func (f *fakeArchiveRepo) EnumerateWorkspaceMonths(_ context.Context, cutoff time.Time) ([]WorkspaceAuditMonth, error) {
	if err := f.consumeFailure("EnumerateWorkspaceMonths"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	type key struct {
		ws uuid.UUID
		ym string
	}
	counts := make(map[key]int)
	for _, r := range f.rows {
		if r.CreatedAt.After(cutoff) || r.CreatedAt.Equal(cutoff) {
			continue
		}
		k := key{ws: r.WorkspaceID, ym: r.CreatedAt.UTC().Format("2006-01")}
		counts[k]++
	}
	var out []WorkspaceAuditMonth
	for k, n := range counts {
		out = append(out, WorkspaceAuditMonth{WorkspaceID: k.ws, YearMonth: k.ym, RowCount: n})
	}
	// Sort stably (workspace_id, year_month) so the archiver
	// sees the same order across runs — matches production
	// SQL ORDER BY.
	sortMonths(out)
	return out, nil
}

func (f *fakeArchiveRepo) FetchBatch(_ context.Context, workspaceID uuid.UUID, yearMonth string, cutoff time.Time, limit int, after uuid.UUID) ([]*Entry, error) {
	if err := f.consumeFailure("FetchBatch"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Production PostgresArchiveRepository.FetchBatch sorts by id ASC
	// and paginates via id > $after. Mirror that contract here:
	// iterate over a copy sorted by UUID-string and apply the cursor
	// in the same order. Iterating in insertion order while filtering
	// by UUID-string comparison would produce inconsistent pagination
	// when row IDs aren't generated in lexicographic order (which is
	// the case for uuid.New() in tests).
	candidates := make([]*Entry, 0, len(f.rows))
	for _, r := range f.rows {
		if f.deleted[r.ID] {
			continue
		}
		if r.WorkspaceID != workspaceID {
			continue
		}
		if r.CreatedAt.After(cutoff) || r.CreatedAt.Equal(cutoff) {
			continue
		}
		if r.CreatedAt.UTC().Format("2006-01") != yearMonth {
			continue
		}
		if after != uuid.Nil && r.ID.String() <= after.String() {
			continue
		}
		candidates = append(candidates, r)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ID.String() < candidates[j].ID.String()
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}

func (f *fakeArchiveRepo) DeleteBatch(_ context.Context, workspaceID uuid.UUID, ids []uuid.UUID) (int, error) {
	if err := f.consumeFailure("DeleteBatch"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, id := range ids {
		for _, r := range f.rows {
			if r.ID == id && r.WorkspaceID == workspaceID && !f.deleted[id] {
				f.deleted[id] = true
				count++
			}
		}
	}
	return count, nil
}

func (f *fakeArchiveRepo) RecordRun(_ context.Context, rec *ArchiveRunRecord) error {
	if err := f.consumeFailure("RecordRun"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copyRec := *rec
	f.runs = append(f.runs, &copyRec)
	return nil
}

func (f *fakeArchiveRepo) ListRuns(_ context.Context, workspaceID uuid.UUID) ([]*ArchiveRunRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*ArchiveRunRecord, 0)
	for _, r := range f.runs {
		if r.WorkspaceID == workspaceID {
			out = append(out, r)
		}
	}
	return out, nil
}

func sortMonths(ms []WorkspaceAuditMonth) {
	// Stable sort by (workspace_id, year_month) — matches the
	// archive enumeration ORDER BY.
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0; j-- {
			if less(ms[j-1], ms[j]) {
				break
			}
			ms[j-1], ms[j] = ms[j], ms[j-1]
		}
	}
}

func less(a, b WorkspaceAuditMonth) bool {
	if a.WorkspaceID != b.WorkspaceID {
		return a.WorkspaceID.String() < b.WorkspaceID.String()
	}
	return a.YearMonth < b.YearMonth
}

// fakeStorage is an in-memory ArchiveStorage that records every
// PutObject call so tests can assert object key shapes and JSONL.gz
// payload contents. The failures map keys are object keys; the
// special key "*" matches any object and lets a test pre-stub a
// failure for the FIRST PutObject call without knowing the
// UUID-suffixed key in advance.
type fakeStorage struct {
	mu        sync.Mutex
	objects   map[string][]byte
	failures  map[string]error
	putCount  int
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objects: make(map[string][]byte), failures: make(map[string]error)}
}

func (s *fakeStorage) PutObject(_ context.Context, objectKey, _ string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCount++
	if err, ok := s.failures[objectKey]; ok {
		delete(s.failures, objectKey)
		return err
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	s.objects[objectKey] = cp
	return nil
}

func (s *fakeStorage) decodeJSONLGz(t *testing.T, key string) []*Entry {
	t.Helper()
	s.mu.Lock()
	body, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("object %q missing", key)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var out []*Entry
	for {
		e := &Entry{}
		if err := dec.Decode(e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func makeEntry(t *testing.T, workspaceID uuid.UUID, createdAt time.Time) *Entry {
	t.Helper()
	return &Entry{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		Action:      ActionLogin,
		CreatedAt:   createdAt,
	}
}

func newTestService(t *testing.T, repo ArchiveRepository, storage ArchiveStorage) *ArchiveService {
	t.Helper()
	svc, err := NewArchiveService(repo, storage, ArchiveServiceConfig{
		RetentionDays:   30,
		ArchivePrefix:   "audit-archive/",
		MaxRowsPerBatch: 100,
	})
	if err != nil {
		t.Fatalf("NewArchiveService: %v", err)
	}
	return svc
}

func TestArchiveService_Run_HappyPath(t *testing.T) {
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45) // 45 days old, before 30-day cutoff
	recent := now.AddDate(0, 0, -10) // 10 days old, after cutoff

	rows := []*Entry{
		makeEntry(t, ws, old),
		makeEntry(t, ws, old.Add(time.Hour)),
		makeEntry(t, ws, old.Add(2*time.Hour)),
		makeEntry(t, ws, recent),
	}

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)
	svc.nowFn = func() time.Time { return now }

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WorkspaceMonthsTotal != 1 {
		t.Fatalf("WorkspaceMonthsTotal = %d, want 1", result.WorkspaceMonthsTotal)
	}
	if result.WorkspaceMonthsOK != 1 {
		t.Fatalf("WorkspaceMonthsOK = %d, want 1", result.WorkspaceMonthsOK)
	}
	if result.RowsArchived != 3 {
		t.Fatalf("RowsArchived = %d, want 3", result.RowsArchived)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", result.Errors)
	}

	// Old rows deleted; recent row preserved.
	for i, r := range rows {
		shouldDelete := r.CreatedAt.Before(old.Add(24 * time.Hour))
		got := repo.deleted[r.ID]
		if got != shouldDelete {
			t.Errorf("rows[%d].deleted = %v, want %v", i, got, shouldDelete)
		}
	}

	if len(repo.runs) != 1 {
		t.Fatalf("runs recorded = %d, want 1", len(repo.runs))
	}
	rec := repo.runs[0]
	if rec.WorkspaceID != ws {
		t.Errorf("run workspace = %v, want %v", rec.WorkspaceID, ws)
	}
	if rec.RowsArchived != 3 {
		t.Errorf("run rows = %d, want 3", rec.RowsArchived)
	}
	if !strings.HasPrefix(rec.ArchiveObjectKey, "audit-archive/"+ws.String()+"/2024-05/") {
		t.Errorf("object key = %q, want prefix audit-archive/%s/2024-05/", rec.ArchiveObjectKey, ws)
	}
	if !strings.HasSuffix(rec.ArchiveObjectKey, ".jsonl.gz") {
		t.Errorf("object key suffix = %q, want .jsonl.gz", rec.ArchiveObjectKey)
	}

	// Decoded JSONL.gz contains exactly the three archived rows.
	decoded := store.decodeJSONLGz(t, rec.ArchiveObjectKey)
	if len(decoded) != 3 {
		t.Fatalf("decoded rows = %d, want 3", len(decoded))
	}
}

func TestArchiveService_Run_RejectsLowRetention(t *testing.T) {
	_, err := NewArchiveService(newFakeRepo(nil), newFakeStorage(), ArchiveServiceConfig{
		RetentionDays: 3,
		ArchivePrefix: "audit-archive/",
	})
	if err == nil {
		t.Fatalf("NewArchiveService allowed retention=3; want error")
	}
	if !strings.Contains(err.Error(), "retention must be >=") {
		t.Errorf("err = %v, want retention floor message", err)
	}
}

// TestArchiveService_archiveWorkspace_TimeoutAttributesRemainingMonths
// is the regression test for the WS-23 PR #68 yellow-flag finding
// (BUG_pr-review-job-92fe43f0a26c44ea817db9bacbc6c88d_0002): when
// WorkspaceTimeout fired mid-loop, the previous workspaceMonthsProcessed
// helper summed the RUN-LEVEL WorkspaceMonthsOK + WorkspaceMonthsFailed
// counters, which are cumulative across ALL workspaces — so months
// processed by an earlier workspace inflated the "processed" count for
// this workspace and undercounted its timed-out remainder. The fix
// tracks a local per-workspace wsProcessed counter inside
// archiveWorkspace. This test exercises a 2-workspace, 3-month-each
// layout where workspace 1 finishes all 3 months OK; then workspace 2
// has its context already cancelled at function entry so all 3 of its
// months should land in WorkspaceMonthsFailed.
func TestArchiveService_archiveWorkspace_TimeoutAttributesRemainingMonths(t *testing.T) {
	ws1 := uuid.New()
	ws2 := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)

	repo := newFakeRepo(nil)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)
	svc.nowFn = func() time.Time { return now }

	// Pre-fill the result as if workspace 1 had completed 3 months
	// successfully (run-level counters are nonzero before workspace
	// 2 starts). The bug would observe these counters when computing
	// workspace 2's unprocessed remainder.
	result := &RunResult{
		RunID:                uuid.New(),
		CutoffTime:           old,
		WorkspaceMonthsOK:    3, // ws1's 3 months
		WorkspaceMonthsTotal: 6,
	}

	// Workspace 2 has 3 months pending; we cancel the parent ctx
	// immediately so the wsCtx.Err() check fires before the first
	// archiveBucket call. All 3 ws2 months must be attributed to
	// Failed, not undercounted as 0 (which is what the bug
	// produced when ws1's OK=3 made the helper return processed=3
	// and thus remaining = 3 - 3 = 0).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	months := []WorkspaceAuditMonth{
		{WorkspaceID: ws2, YearMonth: "2024-01", RowCount: 1},
		{WorkspaceID: ws2, YearMonth: "2024-02", RowCount: 1},
		{WorkspaceID: ws2, YearMonth: "2024-03", RowCount: 1},
	}
	svc.archiveWorkspace(ctx, result.RunID, old, ws2, months, result)

	if result.WorkspaceMonthsFailed != 3 {
		t.Fatalf("WorkspaceMonthsFailed = %d, want 3 (all ws2 months attributed to timeout)", result.WorkspaceMonthsFailed)
	}
	if result.WorkspaceMonthsOK != 3 {
		t.Errorf("WorkspaceMonthsOK = %d, want 3 (ws1's pre-existing count unchanged)", result.WorkspaceMonthsOK)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1 timeout error", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Error(), "3 month(s) unprocessed") {
		t.Errorf("err = %v, want '3 month(s) unprocessed' attribution", result.Errors[0])
	}
	_ = ws1 // ws1 only appears in the result counters
}

func TestArchiveService_Run_NoRowsEligible(t *testing.T) {
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	rows := []*Entry{makeEntry(t, ws, now.AddDate(0, 0, -10))} // within retention

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)
	svc.nowFn = func() time.Time { return now }

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WorkspaceMonthsTotal != 0 || result.RowsArchived != 0 {
		t.Fatalf("got total=%d rows=%d, want 0/0", result.WorkspaceMonthsTotal, result.RowsArchived)
	}
	if store.putCount != 0 {
		t.Errorf("storage.putCount = %d, want 0", store.putCount)
	}
	if len(repo.runs) != 0 {
		t.Errorf("runs recorded = %d, want 0", len(repo.runs))
	}
	if repo.deleted[rows[0].ID] {
		t.Errorf("recent row was deleted")
	}
}

func TestArchiveService_Run_S3UploadFailureLeavesHotRows(t *testing.T) {
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)
	rows := []*Entry{makeEntry(t, ws, old), makeEntry(t, ws, old.Add(time.Hour))}

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)
	svc.nowFn = func() time.Time { return now }

	// Force the FIRST PutObject call to fail. We don't know the
	// final object key in advance (UUID-suffixed), so wrap the
	// fake in a failingFirstPut decorator that returns an error
	// on the first call and then delegates to the inner fake.
	failingStore := &failingFirstPut{inner: store}
	svc.storage = failingStore

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WorkspaceMonthsFailed != 1 {
		t.Fatalf("WorkspaceMonthsFailed = %d, want 1", result.WorkspaceMonthsFailed)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	// Rows still in hot tier — no deletes.
	for _, r := range rows {
		if repo.deleted[r.ID] {
			t.Errorf("row %v deleted despite S3 failure", r.ID)
		}
	}
	// No run record either.
	if len(repo.runs) != 0 {
		t.Errorf("runs recorded = %d, want 0 (no successful upload)", len(repo.runs))
	}
}

type failingFirstPut struct {
	inner *fakeStorage
	once  bool
}

func (f *failingFirstPut) PutObject(ctx context.Context, key, ct string, body []byte) error {
	if !f.once {
		f.once = true
		return errors.New("simulated S3 transient")
	}
	return f.inner.PutObject(ctx, key, ct, body)
}

func TestArchiveService_Run_DeleteFailureProducesPartialSuccess(t *testing.T) {
	// The DELETE step in archiveBucket runs AFTER RecordRun
	// commits, so a delete failure means: S3 object exists,
	// run record exists, hot rows still present. The next run
	// will see the same rows and re-upload them under a new
	// run_id; the cold tier has a duplicate object. We assert
	// that the bucket returns an error and the rows ARE NOT
	// counted as successfully archived in the metrics aggregate.
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)
	rows := []*Entry{makeEntry(t, ws, old)}

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)
	svc.nowFn = func() time.Time { return now }

	repo.injectFailure("DeleteBatch", errors.New("simulated DB error"))

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WorkspaceMonthsFailed != 1 {
		t.Fatalf("WorkspaceMonthsFailed = %d, want 1", result.WorkspaceMonthsFailed)
	}
	if repo.deleted[rows[0].ID] {
		t.Errorf("row was unexpectedly deleted despite DeleteBatch failure")
	}
	// The run record DID get inserted before DELETE failed.
	if len(repo.runs) != 1 {
		t.Errorf("runs recorded = %d, want 1", len(repo.runs))
	}
	// And the S3 object WAS uploaded.
	if store.putCount != 1 {
		t.Errorf("store.putCount = %d, want 1", store.putCount)
	}
}

func TestArchiveService_Run_MultipleWorkspaces(t *testing.T) {
	ws1 := uuid.New()
	ws2 := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)

	rows := []*Entry{
		makeEntry(t, ws1, old),
		makeEntry(t, ws1, old.Add(time.Hour)),
		makeEntry(t, ws2, old),
	}

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)
	svc.nowFn = func() time.Time { return now }

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WorkspaceMonthsTotal != 2 {
		t.Fatalf("WorkspaceMonthsTotal = %d, want 2", result.WorkspaceMonthsTotal)
	}
	if result.RowsArchived != 3 {
		t.Fatalf("RowsArchived = %d, want 3", result.RowsArchived)
	}
	if len(repo.runs) != 2 {
		t.Fatalf("runs recorded = %d, want 2", len(repo.runs))
	}
}

func TestArchiveService_buildObjectKey_Format(t *testing.T) {
	svc, err := NewArchiveService(newFakeRepo(nil), newFakeStorage(), ArchiveServiceConfig{
		RetentionDays: 90,
		ArchivePrefix: "audit-archive/",
	})
	if err != nil {
		t.Fatal(err)
	}
	ws := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	batchID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	got := svc.buildObjectKey(ws, "2024-03", batchID)
	want := "audit-archive/11111111-2222-3333-4444-555555555555/2024-03/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl.gz"
	if got != want {
		t.Errorf("buildObjectKey = %q, want %q", got, want)
	}
}

// TestArchiveService_Run_MultipleBatchesProduceDistinctKeys is the
// regression test for the WS-23 PR #68 critical bug
// (BUG_pr-review-job-92fe43f0a26c44ea817db9bacbc6c88d_0001):
// when one (workspace, month) bucket exceeded MaxRowsPerBatch, the
// archiveBucket loop used to call buildObjectKey with the run-level
// runID for every page, so the second page silently overwrote the
// first page's S3 object while page 1's rows had already been
// deleted from the hot tier (permanent audit data loss). The fix
// generates a fresh batchID per loop iteration and threads it into
// the S3 key. This test pins three invariants:
//
//   - Multiple PutObject calls happen (one per page).
//   - Each PutObject lands at a DISTINCT S3 key.
//   - All input rows survive into the union of all uploaded objects
//     (decoded payloads cover every original row id) — i.e. no page
//     was overwritten in S3.
func TestArchiveService_Run_MultipleBatchesProduceDistinctKeys(t *testing.T) {
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)

	// 5 rows in the same (workspace, month) bucket, all archive-
	// eligible. With MaxRowsPerBatch=2 below, this forces three
	// FetchBatch iterations (2 + 2 + 1) inside archiveBucket.
	rows := []*Entry{
		makeEntry(t, ws, old.Add(0*time.Minute)),
		makeEntry(t, ws, old.Add(1*time.Minute)),
		makeEntry(t, ws, old.Add(2*time.Minute)),
		makeEntry(t, ws, old.Add(3*time.Minute)),
		makeEntry(t, ws, old.Add(4*time.Minute)),
	}

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc, err := NewArchiveService(repo, store, ArchiveServiceConfig{
		RetentionDays:   30,
		ArchivePrefix:   "audit-archive/",
		MaxRowsPerBatch: 2,
	})
	if err != nil {
		t.Fatalf("NewArchiveService: %v", err)
	}
	svc.nowFn = func() time.Time { return now }

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsArchived != len(rows) {
		t.Fatalf("RowsArchived = %d, want %d", result.RowsArchived, len(rows))
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", result.Errors)
	}

	// Three PUTs (2+2+1). Each must land at a distinct S3 key.
	if store.putCount != 3 {
		t.Fatalf("store.putCount = %d, want 3", store.putCount)
	}
	store.mu.Lock()
	keys := make([]string, 0, len(store.objects))
	for k := range store.objects {
		keys = append(keys, k)
	}
	store.mu.Unlock()
	if len(keys) != 3 {
		t.Fatalf("distinct object keys = %d, want 3 (pages collided in S3): %v", len(keys), keys)
	}

	// Every input row must be present in exactly one uploaded
	// object — i.e. no page was overwritten by a subsequent PUT.
	seen := make(map[uuid.UUID]int)
	for _, k := range keys {
		for _, e := range store.decodeJSONLGz(t, k) {
			seen[e.ID]++
		}
	}
	if len(seen) != len(rows) {
		t.Fatalf("unique rows decoded = %d, want %d (data loss across batches): seen = %v", len(seen), len(rows), seen)
	}
	for _, r := range rows {
		if seen[r.ID] != 1 {
			t.Errorf("row %v decoded %d times across all archive objects, want exactly 1", r.ID, seen[r.ID])
		}
	}

	// Each batch must also produce its own audit_log_archive_runs
	// row carrying that batch's S3 key — otherwise the orphan-
	// sweep query (ListObjects minus archive_object_key) would
	// false-positive every successful page beyond the first.
	if len(repo.runs) != 3 {
		t.Fatalf("runs recorded = %d, want 3 (one per batch)", len(repo.runs))
	}
	runKeys := make(map[string]bool, 3)
	for _, r := range repo.runs {
		runKeys[r.ArchiveObjectKey] = true
	}
	for _, k := range keys {
		if !runKeys[k] {
			t.Errorf("S3 key %q has no matching audit_log_archive_runs row", k)
		}
	}
}

func TestEncodeJSONLGzip_RoundTrip(t *testing.T) {
	ws := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entries := []*Entry{
		{ID: uuid.New(), WorkspaceID: ws, Action: ActionLogin, CreatedAt: now},
		{ID: uuid.New(), WorkspaceID: ws, Action: ActionLogout, CreatedAt: now.Add(time.Minute)},
	}
	body, byteCount, err := encodeJSONLGzip(entries)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if byteCount <= 0 {
		t.Errorf("byteCount = %d, want > 0", byteCount)
	}

	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var got []*Entry
	for {
		e := &Entry{}
		if err := dec.Decode(e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		got = append(got, e)
	}
	if len(got) != len(entries) {
		t.Fatalf("decoded len = %d, want %d", len(got), len(entries))
	}
	for i, e := range got {
		if e.ID != entries[i].ID {
			t.Errorf("entry[%d].ID = %v, want %v", i, e.ID, entries[i].ID)
		}
		if !e.CreatedAt.Equal(entries[i].CreatedAt) {
			t.Errorf("entry[%d].CreatedAt = %v, want %v", i, e.CreatedAt, entries[i].CreatedAt)
		}
	}
}
