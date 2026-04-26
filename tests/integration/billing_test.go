package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/billing"
)

// confirmUploadHelper goes through upload-url + confirm-upload to land
// a file with sizeBytes recorded in the files table. Used by billing
// tests so quota checks have non-zero usage to compare against.
func confirmUploadHelper(t *testing.T, env *testEnv, token string, folderID uuid.UUID, name, mime string, sizeBytes int64) uuid.UUID {
	t.Helper()
	status, body := env.httpRequest(http.MethodPost, "/api/files/upload-url", token, map[string]string{
		"folder_id": folderID.String(),
		"filename":  name,
		"mime_type": mime,
	})
	if status != http.StatusOK {
		t.Fatalf("upload-url: status=%d body=%s", status, string(body))
	}
	var ur struct {
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
	}
	env.decodeJSON(body, &ur)

	status, body = env.httpRequest(http.MethodPost, "/api/files/confirm-upload", token, map[string]any{
		"file_id":    ur.UploadID.String(),
		"object_key": ur.ObjectKey,
		"size_bytes": sizeBytes,
		"checksum":   "",
	})
	if status != http.StatusOK {
		t.Fatalf("confirm-upload: status=%d body=%s", status, string(body))
	}
	return ur.UploadID
}

func TestStorageQuotaBlocksUpload(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	// Land a 4 KiB file so usage is well above the limit we set next.
	confirmUploadHelper(t, env, tok.Token, fold.ID, "first.txt", "text/plain", 4096)

	// Force a sub-1 KiB cap so the next upload-url call is over quota.
	var lowLimit int64 = 1024
	status, body := env.httpRequest(http.MethodPut, "/api/admin/billing/plan", tok.Token, map[string]any{
		"tier":              billing.TierFree,
		"max_storage_bytes": lowLimit,
	})
	if status != http.StatusOK {
		t.Fatalf("update plan: status=%d body=%s", status, string(body))
	}

	status, body = env.httpRequest(http.MethodPost, "/api/files/upload-url", tok.Token, map[string]string{
		"folder_id": fold.ID.String(),
		"filename":  "huge.bin",
		"mime_type": "application/octet-stream",
	})
	if status != http.StatusPaymentRequired {
		t.Fatalf("expected 402 over-quota, got %d body=%s", status, string(body))
	}
}

func TestUserQuotaBlocksInvite(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Workspace already has 1 user (the admin); cap at 1 so the next
	// invite must fail.
	one := 1
	status, body := env.httpRequest(http.MethodPut, "/api/admin/billing/plan", tok.Token, map[string]any{
		"tier":      billing.TierFree,
		"max_users": one,
	})
	if status != http.StatusOK {
		t.Fatalf("update plan: status=%d body=%s", status, string(body))
	}

	status, body = env.httpRequest(http.MethodPost, "/api/admin/users", tok.Token, map[string]string{
		"email":    "bob@acme.test",
		"name":     "Bob",
		"password": "pw-bob",
		"role":     "member",
	})
	if status != http.StatusPaymentRequired {
		t.Fatalf("expected 402 over user quota, got %d body=%s", status, string(body))
	}
}

func TestBandwidthMeteringRecordsEvent(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	const fileSize int64 = 2048
	fileID := confirmUploadHelper(t, env, tok.Token, fold.ID, "report.txt", "text/plain", fileSize)

	// Pull a download URL so the handler records a bandwidth event.
	status, body := env.httpRequest(http.MethodGet, "/api/files/"+fileID.String()+"/download-url", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("download-url: status=%d body=%s", status, string(body))
	}

	status, body = env.httpRequest(http.MethodGet, "/api/admin/billing/usage", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("billing usage: status=%d body=%s", status, string(body))
	}
	var summary billing.UsageSummary
	env.decodeJSON(body, &summary)
	if summary.BandwidthUsed < fileSize {
		t.Fatalf("expected bandwidth_used >= %d, got %d", fileSize, summary.BandwidthUsed)
	}
	if summary.StorageUsed != fileSize {
		t.Errorf("expected storage_used=%d, got %d", fileSize, summary.StorageUsed)
	}
}
