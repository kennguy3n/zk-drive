package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/sharing"
)

// TestPhase3DecisionGate exercises the metadata-plane half of the
// Phase 3 decision gate scenario:
//
//	"a paying SME customer can sign up, create a workspace, upload
//	files, share with guests, and the admin can view audit logs and
//	set retention policies."
//
// Coverage:
//   - signup creates a workspace + admin user
//   - admin creates a folder
//   - admin requests a presigned upload URL and confirms the upload
//     metadata (the byte path is intentionally skipped — see note
//     below)
//   - admin invites a guest and the guest accepts the invite
//   - admin reads /api/admin/audit-log
//   - admin upserts a retention policy via /api/admin/retention-policies
//
// NOTE on the byte path: the full round-trip from presign-PUT through
// confirm-upload to a downloadable presigned-GET is blocked on the
// upstream zk-object-fabric query-string SigV4 deferral (Phase 4
// follow-up). We assert here on the *metadata* writes the gate
// implies; an integration test that exercises the actual S3 bytes
// will be wired alongside the upstream presigned-URL fix.
func TestPhase3DecisionGate(t *testing.T) {
	env := setupEnv(t)

	admin := env.signupAndLogin("Acme SME", "admin@acme.test", "Alice", "pw")

	shared := createFolder(t, env, admin.Token, nil, "Shared")

	// Upload metadata path: request URL + confirm. We do not PUT
	// bytes because httptest doesn't run a fabric gateway; the gate
	// scenario is proven end-to-end at the metadata layer when the
	// FileVersion row materialises.
	status, body := env.httpRequest(http.MethodPost, "/api/files/upload-url", admin.Token, map[string]string{
		"folder_id": shared.ID.String(),
		"filename":  "contract.pdf",
		"mime_type": "application/pdf",
	})
	if status != http.StatusOK {
		t.Fatalf("upload-url: status=%d body=%s", status, string(body))
	}
	var urlResp struct {
		UploadID  uuid.UUID `json:"upload_id"`
		ObjectKey string    `json:"object_key"`
	}
	env.decodeJSON(body, &urlResp)

	status, body = env.httpRequest(http.MethodPost, "/api/files/confirm-upload", admin.Token, map[string]any{
		"file_id":    urlResp.UploadID.String(),
		"object_key": urlResp.ObjectKey,
		"size_bytes": 4096,
		"checksum":   "sha256:phase3gate",
	})
	if status != http.StatusOK {
		t.Fatalf("confirm-upload: status=%d body=%s", status, string(body))
	}

	// Guest invite -> accept. Mirrors the existing TestE2ESharingFlow
	// shape (the guest endpoint authenticates against the inviting
	// admin's token in tests because the real out-of-band email link
	// is not wired here).
	expires := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	status, body = env.httpRequest(http.MethodPost, "/api/guest-invites", admin.Token, createGuestInvitePayload{
		Email:     "guest@example.test",
		FolderID:  shared.ID.String(),
		Role:      "viewer",
		ExpiresAt: &expires,
	})
	if status != http.StatusCreated {
		t.Fatalf("create guest invite: status=%d body=%s", status, string(body))
	}
	var inv sharing.GuestInvite
	env.decodeJSON(body, &inv)

	status, body = env.httpRequest(http.MethodPost, "/api/guest-invites/"+inv.ID.String()+"/accept", admin.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("accept guest invite: status=%d body=%s", status, string(body))
	}

	// Admin reads the audit log. The gate only requires that the
	// endpoint succeed and return a list — the exact set of recorded
	// actions is covered by the audit package's own tests.
	status, body = env.httpRequest(http.MethodGet, "/api/admin/audit-log", admin.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("audit-log: status=%d body=%s", status, string(body))
	}
	var auditResp struct {
		Entries []map[string]any `json:"entries"`
	}
	env.decodeJSON(body, &auditResp)
	if auditResp.Entries == nil {
		t.Fatal("audit-log: entries field missing from response")
	}

	// Admin sets a retention policy on the shared folder. The
	// 30-day max-age is arbitrary; the gate is whether the workspace
	// admin can configure retention at all.
	maxAge := 30
	status, body = env.httpRequest(http.MethodPost, "/api/admin/retention-policies", admin.Token, map[string]any{
		"folder_id":    shared.ID.String(),
		"max_age_days": &maxAge,
	})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("upsert retention policy: status=%d body=%s", status, string(body))
	}

	// Verify the policy round-trips through GET.
	status, body = env.httpRequest(http.MethodGet, "/api/admin/retention-policies", admin.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list retention policies: status=%d body=%s", status, string(body))
	}
	var listResp struct {
		Policies []map[string]any `json:"policies"`
	}
	env.decodeJSON(body, &listResp)
	if len(listResp.Policies) == 0 {
		t.Fatal("retention policy not persisted")
	}
}
