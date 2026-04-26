package integration

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/retention"
)

// addAdditionalVersion calls confirm-upload with an existing file_id and
// a hand-crafted object_key (matching the workspaceID/fileID/ prefix
// expected by ConfirmUpload). This is the test shortcut for landing a
// second version of an existing file without going through upload-url —
// the public API doesn't expose a "version-to-existing-file" upload.
func addAdditionalVersion(t *testing.T, env *testEnv, token string, workspaceID, fileID uuid.UUID, sizeBytes int64) {
	t.Helper()
	objectKey := workspaceID.String() + "/" + fileID.String() + "/" + uuid.NewString()
	status, body := env.httpRequest(http.MethodPost, "/api/files/confirm-upload", token, map[string]any{
		"file_id":    fileID.String(),
		"object_key": objectKey,
		"size_bytes": sizeBytes,
	})
	if status != http.StatusOK {
		t.Fatalf("confirm new version: status=%d body=%s", status, string(body))
	}
}

func TestRetentionPolicyCRUD(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	maxAge := 30
	status, body := env.httpRequest(http.MethodPost, "/api/admin/retention-policies", tok.Token, map[string]any{
		"max_age_days": maxAge,
	})
	if status != http.StatusOK {
		t.Fatalf("upsert policy: status=%d body=%s", status, string(body))
	}
	var created retention.Policy
	env.decodeJSON(body, &created)
	if created.ID == uuid.Nil {
		t.Fatal("expected non-nil policy id")
	}

	status, body = env.httpRequest(http.MethodGet, "/api/admin/retention-policies", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list policies: status=%d", status)
	}
	var list struct {
		Policies []retention.Policy `json:"policies"`
	}
	env.decodeJSON(body, &list)
	if len(list.Policies) != 1 || list.Policies[0].ID != created.ID {
		t.Fatalf("expected created policy in list, got %+v", list.Policies)
	}

	status, _ = env.httpRequest(http.MethodDelete, "/api/admin/retention-policies/"+created.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete policy: expected 204, got %d", status)
	}

	status, body = env.httpRequest(http.MethodGet, "/api/admin/retention-policies", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list after delete: status=%d", status)
	}
	env.decodeJSON(body, &list)
	if len(list.Policies) != 0 {
		t.Fatalf("expected empty list after delete, got %d", len(list.Policies))
	}
}

func TestEvaluateReturnsExpiredVersions(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	wsID, err := uuid.Parse(tok.WorkspaceID)
	if err != nil {
		t.Fatalf("parse workspace_id: %v", err)
	}
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	// Two versions on the same file: v1 is the "old" one we want
	// flagged; v2 becomes current after the second confirm.
	fileID := confirmUploadHelper(t, env, tok.Token, fold.ID, "memo.txt", "text/plain", 100)
	addAdditionalVersion(t, env, tok.Token, wsID, fileID, 200)

	// Backdate v1 so it's older than the policy window. We target
	// the non-current version explicitly so the SQL filter
	// `v.id <> current_version_id` keeps it eligible for delete.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = env.pool.Exec(ctx, `
UPDATE file_versions
SET created_at = now() - interval '10 days'
WHERE file_id = $1 AND id <> (SELECT current_version_id FROM files WHERE id = $1)`, fileID)
	if err != nil {
		t.Fatalf("backdate version: %v", err)
	}

	maxAge := 1
	status, _ := env.httpRequest(http.MethodPost, "/api/admin/retention-policies", tok.Token, map[string]any{
		"max_age_days": maxAge,
	})
	if status != http.StatusOK {
		t.Fatalf("create policy: status=%d", status)
	}

	svc := retention.NewService(retention.NewPostgresRepository(env.pool), env.pool)
	result, err := svc.Evaluate(ctx, wsID, time.Now().UTC())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(result.DeleteVersions) != 1 {
		t.Fatalf("expected 1 expired version, got %d (%v)", len(result.DeleteVersions), result.DeleteVersions)
	}
}

func TestColdArchiveWritesGzipObject(t *testing.T) {
	if os.Getenv("S3_ENDPOINT") == "" {
		t.Skip("S3_ENDPOINT not set; cold-archive round-trip needs a live object store")
	}
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	wsID, _ := uuid.Parse(tok.WorkspaceID)
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	fileID := confirmUploadHelper(t, env, tok.Token, fold.ID, "report.txt", "text/plain", 0)
	addAdditionalVersion(t, env, tok.Token, wsID, fileID, 0)

	// Pick the oldest (non-current) version row and archive it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var versionID uuid.UUID
	row := env.pool.QueryRow(ctx, `
SELECT id FROM file_versions
WHERE file_id = $1 AND id <> (SELECT current_version_id FROM files WHERE id = $1)
LIMIT 1`, fileID)
	if err := row.Scan(&versionID); err != nil {
		t.Fatalf("locate non-current version: %v", err)
	}

	archive := retention.NewArchiveService(env.pool, env.storage, nil)
	if err := archive.ArchiveVersion(ctx, versionID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	var archivedAt *time.Time
	if err := env.pool.QueryRow(ctx, `SELECT archived_at FROM file_versions WHERE id = $1`, versionID).Scan(&archivedAt); err != nil {
		t.Fatalf("read archived_at: %v", err)
	}
	if archivedAt == nil {
		t.Fatal("expected archived_at to be set after ArchiveVersion")
	}
}
