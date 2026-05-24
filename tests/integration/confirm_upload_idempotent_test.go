package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestConfirmUploadIdempotentReplay pins the contract that POST
// /api/files/confirm-upload is safe to retry with the same payload —
// a property network-flaky clients depend on after a TCP reset
// between a successful PUT-to-S3 and the confirm hop.
//
// Before the versionID-pinning change, replays silently created
// a new file_versions row on each retry (each minted a fresh
// uuid.New()) — N rows pointing at the same S3 object, all but the
// latest orphaned. Pinning v.ID to the object_key's version segment
// exposed the bug as a unique-constraint 500. The fix is for
// insertVersionTx to ON CONFLICT (id) DO NOTHING and re-fetch the
// existing row on conflict, so a legitimate retry returns the same
// success response and no duplicate row is created.
//
// The test counts versions before and after the second confirm — a
// regression to either the old "duplicate row" behaviour or the
// short-lived "500 on retry" behaviour would surface here.
func TestConfirmUploadIdempotentReplay(t *testing.T) {
	env := setupEnv(t)
	env.ResetTables()

	tok := env.signupAndLogin("Idem Co", "idem@example.com", "Idem", "hunter2hunter2")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	status, body := env.httpRequest(http.MethodPost, "/api/files/upload-url", tok.Token, map[string]string{
		"folder_id": fold.ID.String(),
		"filename":  "report.pdf",
		"mime_type": "application/pdf",
	})
	if status != http.StatusOK {
		t.Fatalf("upload-url: status=%d body=%s", status, string(body))
	}
	var urlResp struct {
		UploadURL string    `json:"upload_url"`
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
	}
	env.decodeJSON(body, &urlResp)

	confirm := func() (int, []byte) {
		return env.httpRequest(http.MethodPost, "/api/files/confirm-upload", tok.Token, map[string]any{
			"file_id":    urlResp.UploadID.String(),
			"object_key": urlResp.ObjectKey,
			"size_bytes": 1234,
			"checksum":   "sha256:idem",
		})
	}

	// First call: legit confirm.
	status1, body1 := confirm()
	if status1 != http.StatusOK {
		t.Fatalf("first confirm: status=%d body=%s", status1, string(body1))
	}

	// Capture the file row's updated_at after the first commit so we
	// can assert later that retries do NOT bump it. The replay path
	// in ConfirmVersion explicitly skips the UPDATE because re-
	// issuing `updated_at = now()` would pollute the last-modified
	// timestamp sync clients rely on for change detection.
	var firstUpdatedAt time.Time
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT updated_at FROM files WHERE id = $1`,
		urlResp.UploadID,
	).Scan(&firstUpdatedAt); err != nil {
		t.Fatalf("read updated_at after first confirm: %v", err)
	}

	// Sleep at least one Postgres-clock tick so a buggy "always
	// run the UPDATE" implementation would produce an updated_at
	// value strictly greater than firstUpdatedAt. Without this,
	// a fast-enough machine might run the UPDATE inside the same
	// statement_timestamp() bucket and the assertion would pass
	// vacuously.
	time.Sleep(20 * time.Millisecond)

	// Second call: byte-for-byte identical retry. Must succeed
	// without creating a new version row.
	status2, body2 := confirm()
	if status2 != http.StatusOK {
		t.Fatalf("idempotent retry confirm: status=%d body=%s", status2, string(body2))
	}

	// Verify only ONE file_versions row exists. We hit the DB
	// directly rather than the API because the GET endpoint already
	// folds duplicates by current_version_id — we want to count raw
	// inserts.
	var count int
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT COUNT(*) FROM file_versions WHERE file_id = $1`,
		urlResp.UploadID,
	).Scan(&count); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if count != 1 {
		t.Fatalf("file_versions row count after idempotent retry = %d, want 1", count)
	}

	// Sanity: the third extension — three confirms still yield one
	// row. Makes the property robust against any leftover "second
	// call is special" branch.
	status3, body3 := confirm()
	if status3 != http.StatusOK {
		t.Fatalf("third confirm: status=%d body=%s", status3, string(body3))
	}
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT COUNT(*) FROM file_versions WHERE file_id = $1`,
		urlResp.UploadID,
	).Scan(&count); err != nil {
		t.Fatalf("count versions after third confirm: %v", err)
	}
	if count != 1 {
		t.Fatalf("file_versions row count after third confirm = %d, want 1", count)
	}

	// Side-effect suppression: activity_log and usage_events must
	// also be idempotent — three back-to-back confirms of the same
	// version should produce exactly one `file.upload` activity row
	// and one storage_bytes_uploaded usage_events row. Without the
	// handler's `if fresh { ... }` gate this would be 3-of-each.
	var activityCount int
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT COUNT(*) FROM activity_log WHERE resource_id = $1 AND action = $2`,
		urlResp.UploadID, "file.upload",
	).Scan(&activityCount); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if activityCount != 1 {
		t.Fatalf("activity_log file.upload row count = %d, want 1", activityCount)
	}

	var usageCount int
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT COUNT(*) FROM usage_events WHERE workspace_id = (SELECT workspace_id FROM files WHERE id = $1) AND event_type = $2`,
		urlResp.UploadID, "storage",
	).Scan(&usageCount); err != nil {
		t.Fatalf("count usage_events rows: %v", err)
	}
	if usageCount != 1 {
		t.Fatalf("usage_events storage row count = %d, want 1", usageCount)
	}

	// updated_at must NOT have advanced across the two retry confirms.
	// ConfirmVersion's replay branch skips the UPDATE precisely so
	// the file's last-modified timestamp reflects the real upload,
	// not a network hiccup.
	var lastUpdatedAt time.Time
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT updated_at FROM files WHERE id = $1`,
		urlResp.UploadID,
	).Scan(&lastUpdatedAt); err != nil {
		t.Fatalf("read updated_at after retries: %v", err)
	}
	if !lastUpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("files.updated_at advanced across replays: first=%v last=%v", firstUpdatedAt, lastUpdatedAt)
	}

	// size_bytes must equal the original confirm's value — a buggy
	// replay path that re-issued the UPDATE could overwrite this
	// with stale or zero data depending on the precise scenario.
	var sizeBytes int64
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT size_bytes FROM files WHERE id = $1`,
		urlResp.UploadID,
	).Scan(&sizeBytes); err != nil {
		t.Fatalf("read size_bytes: %v", err)
	}
	if sizeBytes != 1234 {
		t.Fatalf("files.size_bytes after replays = %d, want 1234", sizeBytes)
	}
}

// TestConfirmUploadReplayDoesNotRegressVersion is the harder half of
// the idempotency contract: a retry of V1's confirm MUST NOT silently
// overwrite a newer V2 that another caller has committed in the
// interval. Before the version-pin fix, ConfirmVersion ran its
// UPDATE files SET current_version_id = $3 unconditionally on the
// replay path, so a stale V1 retry would roll back current_version_id
// to V1 and overwrite size_bytes with V1's size — discarding V2's
// work entirely.
//
// The fix is to skip the UPDATE entirely when insertVersionTx returns
// fresh=false (the replay branch). The first commit already advanced
// the file pointer atomically with the version-row insert, so any
// later UPDATE is at best a no-op and at worst (concurrent V2) a
// regression.
//
// The current HTTP API doesn't expose a "new version of existing file"
// endpoint (every UploadURL call mints a fresh file row), so we
// fabricate the V2 scenario by directly inserting a second version row
// and pointing files.current_version_id at it — this matches what an
// internal caller (e.g. KChat attachments via wiring/kchat_adapter)
// can do today and what a future /files/{id}/upload-version endpoint
// would do.
func TestConfirmUploadReplayDoesNotRegressVersion(t *testing.T) {
	env := setupEnv(t)
	env.ResetTables()

	tok := env.signupAndLogin("Race Co", "race@example.com", "Race", "hunter2hunter2")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	// Mint V1's upload-url.
	statusV1, bodyV1 := env.httpRequest(http.MethodPost, "/api/files/upload-url", tok.Token, map[string]string{
		"folder_id": fold.ID.String(),
		"filename":  "report.pdf",
		"mime_type": "application/pdf",
	})
	if statusV1 != http.StatusOK {
		t.Fatalf("upload-url V1: status=%d body=%s", statusV1, string(bodyV1))
	}
	var v1 struct {
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
	}
	env.decodeJSON(bodyV1, &v1)

	// First confirm of V1 succeeds.
	statusFirst, bodyFirst := env.httpRequest(http.MethodPost, "/api/files/confirm-upload", tok.Token, map[string]any{
		"file_id":    v1.UploadID.String(),
		"object_key": v1.ObjectKey,
		"size_bytes": 100,
		"checksum":   "sha256:v1",
	})
	if statusFirst != http.StatusOK {
		t.Fatalf("first confirm V1: status=%d body=%s", statusFirst, string(bodyFirst))
	}

	// Look up the workspace_id and creator for the file so we can
	// directly insert V2 below.
	var (
		wsID      uuid.UUID
		createdBy uuid.UUID
	)
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT workspace_id, created_by FROM files WHERE id = $1`,
		v1.UploadID,
	).Scan(&wsID, &createdBy); err != nil {
		t.Fatalf("read workspace_id / created_by: %v", err)
	}

	// Fabricate V2 directly: this matches what an internal caller
	// (KChat adapter, future upload-version endpoint) would do —
	// insert a new file_versions row and atomically advance
	// files.current_version_id + size_bytes. Doing it via raw SQL
	// keeps the test independent of any future API additions.
	v2VersionID := uuid.New()
	v2ObjectKey := wsID.String() + "/" + v1.UploadID.String() + "/" + v2VersionID.String()
	tx, err := env.pool.Begin(env.t.Context())
	if err != nil {
		t.Fatalf("begin V2 tx: %v", err)
	}
	defer func() { _ = tx.Rollback(env.t.Context()) }()
	if _, err := tx.Exec(env.t.Context(),
		`INSERT INTO file_versions (id, file_id, version_number, object_key, size_bytes, checksum, created_by)
SELECT $1, $2, COALESCE(MAX(version_number), 0) + 1, $3, $4, $5, $6 FROM file_versions WHERE file_id = $2`,
		v2VersionID, v1.UploadID, v2ObjectKey, int64(200), "sha256:v2", createdBy,
	); err != nil {
		t.Fatalf("insert V2 version row: %v", err)
	}
	if _, err := tx.Exec(env.t.Context(),
		`UPDATE files SET current_version_id = $1, size_bytes = $2, updated_at = now() WHERE id = $3`,
		v2VersionID, int64(200), v1.UploadID,
	); err != nil {
		t.Fatalf("advance files.current_version_id to V2: %v", err)
	}
	if err := tx.Commit(env.t.Context()); err != nil {
		t.Fatalf("commit V2 tx: %v", err)
	}

	// Now simulate the stale V1 retry hitting the server (e.g. a
	// client that lost its first V1 confirm's response and only
	// just reconnected to the cell).
	statusReplay, bodyReplay := env.httpRequest(http.MethodPost, "/api/files/confirm-upload", tok.Token, map[string]any{
		"file_id":    v1.UploadID.String(),
		"object_key": v1.ObjectKey,
		"size_bytes": 100,
		"checksum":   "sha256:v1",
	})
	// The replay must succeed (idempotency contract) — but the
	// crucial assertion is the side effect, not the status code.
	if statusReplay != http.StatusOK {
		t.Fatalf("stale V1 replay: status=%d body=%s", statusReplay, string(bodyReplay))
	}

	// REGRESSION CHECK: current_version_id must still point at V2.
	// A buggy replay would set it back to V1.
	var finalVersionID uuid.UUID
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT current_version_id FROM files WHERE id = $1`,
		v1.UploadID,
	).Scan(&finalVersionID); err != nil {
		t.Fatalf("read current_version_id after V1 replay: %v", err)
	}
	if finalVersionID != v2VersionID {
		t.Fatalf("stale V1 replay regressed current_version_id: was %s after V2, now %s", v2VersionID, finalVersionID)
	}

	// And size_bytes must still be V2's 200, not V1's 100.
	var finalSize int64
	if err := env.pool.QueryRow(env.t.Context(),
		`SELECT size_bytes FROM files WHERE id = $1`,
		v1.UploadID,
	).Scan(&finalSize); err != nil {
		t.Fatalf("read size_bytes after V1 replay: %v", err)
	}
	if finalSize != 200 {
		t.Fatalf("stale V1 replay regressed size_bytes: want 200 (V2), got %d", finalSize)
	}
}
