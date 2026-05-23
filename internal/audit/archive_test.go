package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// failureAfterN[method] = N schedules an error on the (N+1)-th
	// call to that method, so a test can let the first N calls run
	// normally and only fail the N+1-th. Useful for pinning
	// partial-page accounting: page 1 succeeds end-to-end, page 2
	// fails at a specific step. Decremented on each call until 0,
	// at which point the failure fires + the entry is cleared.
	failureAfterN map[string]int
	failureAfterErr map[string]error
}

func newFakeRepo(rows []*Entry) *fakeArchiveRepo {
	return &fakeArchiveRepo{
		rows:            rows,
		deleted:         make(map[uuid.UUID]bool),
		failures:        make(map[string]error),
		failureAfterN:   make(map[string]int),
		failureAfterErr: make(map[string]error),
	}
}

func (f *fakeArchiveRepo) injectFailure(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = err
}

// injectFailureAfter schedules an error on the (n+1)-th call to method.
// n=0 fails the very next call (same as injectFailure). n=1 lets the
// first call through and fails the second. Used by
// TestArchiveService_Run_PartialPageSuccessIsAttributed to fail the
// SECOND DeleteBatch (page 2) while letting page 1 complete
// end-to-end — the configuration the partial-page accounting fix
// must handle correctly.
func (f *fakeArchiveRepo) injectFailureAfter(method string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failureAfterN[method] = n
	f.failureAfterErr[method] = err
}

func (f *fakeArchiveRepo) consumeFailure(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failures[method]; ok {
		delete(f.failures, method)
		return err
	}
	if n, ok := f.failureAfterN[method]; ok {
		if n <= 0 {
			err := f.failureAfterErr[method]
			delete(f.failureAfterN, method)
			delete(f.failureAfterErr, method)
			return err
		}
		f.failureAfterN[method] = n - 1
	}
	return nil
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
	monthStart, monthEnd, err := parseYearMonthRange(yearMonth)
	if err != nil {
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
	//
	// Month membership uses the same [monthStart, monthEnd) half-open
	// range the production SQL applies via parseYearMonthRange, so a
	// row created at exactly midnight on the first of a month belongs
	// to that month (and not the prior one's last-second boundary).
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
		created := r.CreatedAt.UTC()
		if created.Before(monthStart) || !created.Before(monthEnd) {
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
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	copyRec := *rec
	f.runs = append(f.runs, &copyRec)
	return nil
}

func (f *fakeArchiveRepo) SetRunError(_ context.Context, id uuid.UUID, errMsg string) error {
	if err := f.consumeFailure("SetRunError"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.runs {
		if f.runs[i].ID == id {
			f.runs[i].ErrorMessage = &errMsg
			return nil
		}
	}
	return fmt.Errorf("fakeArchiveRepo: SetRunError no row with id=%s", id)
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
	// Failure row IS recorded so the partial-failure dashboard
	// (idx_audit_log_archive_runs_failures) surfaces the attempt.
	// rows_archived == 0, bytes_uploaded == 0, error_message != nil.
	// See WS-23 PR #68 Devin Review finding
	// ANALYSIS_pr-review-job-d2a9e87dcd554aae916858730442da4c_0001.
	if len(repo.runs) != 1 {
		t.Fatalf("runs recorded = %d, want 1 (the pre-upload failure row)", len(repo.runs))
	}
	failRec := repo.runs[0]
	if failRec.ErrorMessage == nil {
		t.Errorf("failure row ErrorMessage = nil, want non-nil (so partial-failure dashboard lights up)")
	} else if !strings.Contains(*failRec.ErrorMessage, "simulated S3 transient") {
		t.Errorf("failure row ErrorMessage = %q, want it to wrap the underlying upload error", *failRec.ErrorMessage)
	}
	if failRec.RowsArchived != 0 {
		t.Errorf("failure row RowsArchived = %d, want 0 (no rows landed in cold storage)", failRec.RowsArchived)
	}
	if failRec.BytesUploaded != 0 {
		t.Errorf("failure row BytesUploaded = %d, want 0 (no bytes landed in cold storage)", failRec.BytesUploaded)
	}
	if failRec.ArchiveObjectKey == "" {
		t.Errorf("failure row ArchiveObjectKey is empty; the would-have-been key must be recorded so operators can cross-reference S3 ListObjects")
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
	// The DELETE step in archiveBucket runs AFTER PutObject +
	// RecordRun commit, so a DeleteBatch failure on a single-page
	// bucket means: S3 object exists, archive_runs row exists,
	// hot rows still present. The next run sees the same rows and
	// re-uploads them under a fresh batch_id; the cold tier
	// carries one duplicate object. We assert:
	//
	//   - the bucket is flagged as failed (WorkspaceMonthsFailed=1)
	//   - the row WAS durably committed to cold storage (the row
	//     count + bytes ARE attributed to RowsArchived /
	//     BytesUploaded so operators see honest counters even
	//     when the hot-tier delete failed)
	//   - the hot row is still present (no false delete)
	//   - the audit_log_archive_runs INSERT did happen
	//   - the S3 PUT did happen
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)
	rows := []*Entry{makeEntry(t, ws, old)}

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)
	svc.nowFn = func() time.Time { return now }
	spy := &spyMetricsRecorder{}
	svc.WithMetrics(spy)

	repo.injectFailure("DeleteBatch", errors.New("simulated DB error"))

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WorkspaceMonthsFailed != 1 {
		t.Fatalf("WorkspaceMonthsFailed = %d, want 1", result.WorkspaceMonthsFailed)
	}
	// Fix WS-23 PR #68 finding #1: the page DID upload + record;
	// the rows it represents MUST be counted in the aggregate
	// (otherwise zkdrive_audit_archive_rows_total undercounts
	// actual cold-tier activity).
	if result.RowsArchived != 1 {
		t.Errorf("RowsArchived = %d, want 1 (DELETE failed AFTER S3 + RecordRun committed; rows are in cold storage)", result.RowsArchived)
	}
	if result.BytesUploaded <= 0 {
		t.Errorf("BytesUploaded = %d, want > 0 (the page WAS uploaded)", result.BytesUploaded)
	}
	if repo.deleted[rows[0].ID] {
		t.Errorf("row was unexpectedly deleted despite DeleteBatch failure")
	}
	// The run record DID get inserted before DELETE failed.
	if len(repo.runs) != 1 {
		t.Fatalf("runs recorded = %d, want 1", len(repo.runs))
	}
	// And its error_message column was stamped by SetRunError so
	// the partial-failure dashboard (idx_audit_log_archive_runs_failures)
	// lights up on this DELETE failure even though the upload +
	// row insert both succeeded. See WS-23 PR #68 Devin Review
	// finding ANALYSIS_pr-review-job-d2a9e87dcd554aae916858730442da4c_0001.
	if repo.runs[0].ErrorMessage == nil {
		t.Errorf("run record ErrorMessage = nil after DeleteBatch failure; want non-nil (idx_audit_log_archive_runs_failures relies on this)")
	} else if !strings.Contains(*repo.runs[0].ErrorMessage, "simulated DB error") {
		t.Errorf("run record ErrorMessage = %q, want it to wrap the underlying DeleteBatch error", *repo.runs[0].ErrorMessage)
	}
	// And the S3 object WAS uploaded.
	if store.putCount != 1 {
		t.Errorf("store.putCount = %d, want 1", store.putCount)
	}
	// Bucket metric: the failed-after-commit path emits the
	// "partial" label (rows>0 alongside err), distinguishing the
	// "rows are durably in cold storage" case from the "nothing
	// got committed" case.
	if len(spy.buckets) != 1 {
		t.Fatalf("metric bucket calls = %d, want 1", len(spy.buckets))
	}
	if spy.buckets[0].result != archiveBucketResultPartial {
		t.Errorf("bucket result label = %q, want %q (single-page commit followed by DELETE failure)", spy.buckets[0].result, archiveBucketResultPartial)
	}
	if spy.buckets[0].rows != 1 {
		t.Errorf("bucket metric rows = %d, want 1 (the partial-page count must be passed through, not zeroed)", spy.buckets[0].rows)
	}
}

// TestArchiveService_Run_PartialPageSuccessIsAttributed is the
// regression test for the WS-23 PR #68 finding #1
// (ANALYSIS_pr-review-job-275fde026190462681d85c491dca8a38_0001):
// when a (workspace, month) bucket exceeds MaxRowsPerBatch and
// the bucket fails on page N AFTER pages 1..N-1 have been durably
// committed, the successfully-committed pages' rows and bytes
// MUST be counted in RunResult.RowsArchived / BytesUploaded and
// the bucket metric MUST receive a non-zero rows count with the
// "partial" label \u2014 otherwise operators see a sustained
// undercount in zkdrive_audit_archive_rows_total relative to
// actual cold-tier activity.
func TestArchiveService_Run_PartialPageSuccessIsAttributed(t *testing.T) {
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)

	// 4 rows in one (workspace, month) bucket; MaxRowsPerBatch=2
	// yields two pages (2 + 2). Page 1 completes end-to-end; the
	// SECOND DeleteBatch call fails so page 2 fails mid-flight
	// but page 1 is durably in cold storage.
	rows := []*Entry{
		makeEntry(t, ws, old.Add(0*time.Minute)),
		makeEntry(t, ws, old.Add(1*time.Minute)),
		makeEntry(t, ws, old.Add(2*time.Minute)),
		makeEntry(t, ws, old.Add(3*time.Minute)),
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
	spy := &spyMetricsRecorder{}
	svc.WithMetrics(spy)

	// n=1: first DeleteBatch succeeds, second fails. So page 1
	// goes all the way through (PUT + RecordRun + DELETE); page 2
	// completes PUT + RecordRun but fails at DELETE.
	repo.injectFailureAfter("DeleteBatch", 1, errors.New("simulated DB error on page 2"))

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Bucket is marked failed.
	if result.WorkspaceMonthsFailed != 1 {
		t.Errorf("WorkspaceMonthsFailed = %d, want 1", result.WorkspaceMonthsFailed)
	}
	if result.WorkspaceMonthsOK != 0 {
		t.Errorf("WorkspaceMonthsOK = %d, want 0", result.WorkspaceMonthsOK)
	}

	// Page 1 (2 rows) committed end-to-end: hot rows deleted, S3
	// object present. Page 2 (2 rows) uploaded + recorded but
	// hot rows still present. Both pages' rows + bytes are real
	// cold-tier activity and MUST be counted.
	if result.RowsArchived != 4 {
		t.Errorf("RowsArchived = %d, want 4 (both pages were durably uploaded to S3 + recorded in audit_log_archive_runs before page 2's DELETE failed)", result.RowsArchived)
	}
	if result.BytesUploaded <= 0 {
		t.Errorf("BytesUploaded = %d, want > 0", result.BytesUploaded)
	}

	// Both S3 PUTs happened and produced distinct objects.
	if store.putCount != 2 {
		t.Errorf("store.putCount = %d, want 2 (one per page)", store.putCount)
	}
	if len(repo.runs) != 2 {
		t.Fatalf("runs recorded = %d, want 2 (one per page, both pages reached RecordRun)", len(repo.runs))
	}
	// Page 1's run has no error (DELETE succeeded). Page 2's run
	// has error_message stamped by SetRunError because its
	// DeleteBatch failed AFTER the upload + RecordRun committed.
	// The 2-row layout means the dashboard query
	// `WHERE error_message IS NOT NULL` correctly surfaces just
	// the failed page. Order of repo.runs matches RecordRun call
	// order (page 1 first, then page 2), pinned by fakeArchiveRepo's
	// append-on-insert semantics. See WS-23 PR #68 Devin Review
	// finding ANALYSIS_pr-review-job-d2a9e87dcd554aae916858730442da4c_0001.
	if repo.runs[0].ErrorMessage != nil {
		t.Errorf("page 1 ErrorMessage = %q, want nil (page 1 committed end-to-end)", *repo.runs[0].ErrorMessage)
	}
	if repo.runs[1].ErrorMessage == nil {
		t.Errorf("page 2 ErrorMessage = nil, want non-nil (page 2's DELETE failed after S3 + RecordRun committed)")
	} else if !strings.Contains(*repo.runs[1].ErrorMessage, "simulated DB error on page 2") {
		t.Errorf("page 2 ErrorMessage = %q, want it to wrap the underlying DeleteBatch error", *repo.runs[1].ErrorMessage)
	}

	// Page 1's hot rows were deleted (page 1's DELETE
	// succeeded); page 2's hot rows were NOT deleted (page 2's
	// DELETE failed).
	deletedCount := 0
	for _, r := range rows {
		if repo.deleted[r.ID] {
			deletedCount++
		}
	}
	if deletedCount != 2 {
		t.Errorf("deleted hot rows = %d, want 2 (page 1's 2 rows; page 2's 2 rows must remain in hot tier for next run)", deletedCount)
	}

	// Bucket-level metric: ONE call with label="partial" and
	// rows=4 (all uploaded pages, NOT zero). This is the precise
	// signal an operator watching zkdrive_audit_archive_rows_total
	// vs zkdrive_audit_archive_buckets_total{result="partial"}
	// needs to see honest counter movement on partial-failure
	// runs.
	if len(spy.buckets) != 1 {
		t.Fatalf("metric bucket calls = %d, want 1", len(spy.buckets))
	}
	if spy.buckets[0].result != archiveBucketResultPartial {
		t.Errorf("bucket result label = %q, want %q", spy.buckets[0].result, archiveBucketResultPartial)
	}
	if spy.buckets[0].rows != 4 {
		t.Errorf("bucket metric rows = %d, want 4 (partial pages' rows must NOT be zeroed)", spy.buckets[0].rows)
	}
	if spy.buckets[0].bytes <= 0 {
		t.Errorf("bucket metric bytes = %d, want > 0", spy.buckets[0].bytes)
	}
}

// TestArchiveService_Run_StartedAndCompletedDiffer is the regression
// test for the WS-23 PR #68 finding #3
// (ANALYSIS_pr-review-job-275fde026190462681d85c491dca8a38_0003):
// audit_log_archive_runs.started_at MUST reflect the moment the
// page's processing began (before FetchBatch), and completed_at
// MUST reflect the moment after the S3 upload finished. Capturing
// both at the same instant produced an always-zero-duration column
// that future admin-console / duration dashboards could not use.
func TestArchiveService_Run_StartedAndCompletedDiffer(t *testing.T) {
	ws := uuid.New()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -45)
	rows := []*Entry{makeEntry(t, ws, old)}

	repo := newFakeRepo(rows)
	store := newFakeStorage()
	svc := newTestService(t, repo, store)

	// nowFn returns monotonically increasing instants on each
	// call so we can observe whether the service captures two
	// distinct samples (started + completed) per page.
	var nowCalls int
	svc.nowFn = func() time.Time {
		nowCalls++
		return now.Add(time.Duration(nowCalls) * time.Millisecond)
	}

	result, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RowsArchived != 1 {
		t.Fatalf("RowsArchived = %d, want 1", result.RowsArchived)
	}
	if len(repo.runs) != 1 {
		t.Fatalf("runs recorded = %d, want 1", len(repo.runs))
	}
	rec := repo.runs[0]
	if !rec.CompletedAt.After(rec.StartedAt) {
		t.Errorf("CompletedAt (%s) must be strictly after StartedAt (%s); they were captured as the same instant (zero-duration row)", rec.CompletedAt, rec.StartedAt)
	}
}

// spyMetricsRecorder captures every RecordAuditArchiveBucket call
// so tests can assert per-bucket label + counts (notably for the
// partial-page accounting fix).
type spyMetricsRecorder struct {
	mu      sync.Mutex
	buckets []spyBucketCall
}

type spyBucketCall struct {
	result string
	rows   int
	bytes  int64
}

func (s *spyMetricsRecorder) RecordAuditArchiveBucket(result string, rows int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = append(s.buckets, spyBucketCall{result: result, rows: rows, bytes: bytes})
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

// TestParseYearMonthRange pins the boundary semantics the SQL
// FetchBatch query depends on after the WS-23 PR #68 finding #2
// fix (ANALYSIS_pr-review-job-275fde026190462681d85c491dca8a38_0002):
// the half-open [monthStart, monthEnd) UTC range replacing the
// non-SARGable to_char(date_trunc(...)) predicate must include
// the first instant of the month and exclude the first instant
// of the next month \u2014 anything else silently re-classifies rows
// near the month boundary.
func TestParseYearMonthRange(t *testing.T) {
	cases := []struct {
		yearMonth string
		wantStart time.Time
		wantEnd   time.Time
		wantErr   bool
	}{
		{
			yearMonth: "2024-03",
			wantStart: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// Year rollover: December's monthEnd is the next year's January 1.
			yearMonth: "2024-12",
			wantStart: time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// Leap February: 2024-02 ends at 2024-03-01.
			yearMonth: "2024-02",
			wantStart: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{yearMonth: "2024-13", wantErr: true},
		{yearMonth: "not-a-month", wantErr: true},
		{yearMonth: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.yearMonth, func(t *testing.T) {
			start, end, err := parseYearMonthRange(tc.yearMonth)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseYearMonthRange(%q) err = nil, want error", tc.yearMonth)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseYearMonthRange(%q) err = %v", tc.yearMonth, err)
			}
			if !start.Equal(tc.wantStart) {
				t.Errorf("start = %s, want %s", start, tc.wantStart)
			}
			if !end.Equal(tc.wantEnd) {
				t.Errorf("end = %s, want %s", end, tc.wantEnd)
			}
		})
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
