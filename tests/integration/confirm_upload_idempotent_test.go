package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestConfirmUploadIdempotentReplay pins the contract that POST
// /api/files/confirm-upload is safe to retry with the same payload —
// a property network-flaky clients depend on after a TCP reset
// between a successful PUT-to-S3 and the confirm hop.
//
// Before the WS-3 versionID-pinning change, replays silently created
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
}
