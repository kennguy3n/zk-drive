package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/fabric"
)

// seedFabricCredentials inserts a workspace_storage_credentials row
// for workspaceID so admin endpoints that operate on the row (CMK,
// placement) have something to update. The provisioner.Persist path
// is the same one signup uses in production, so tests exercise the
// real upsert SQL.
func seedFabricCredentials(t *testing.T, env *testEnv, workspaceID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := env.provisioner.Persist(ctx, &fabric.Credentials{
		WorkspaceID:        workspaceID,
		TenantID:           "tenant-" + workspaceID.String(),
		AccessKey:          "test-access-key",
		SecretKey:          "test-secret-key",
		Endpoint:           "http://127.0.0.1:65535",
		Bucket:             "test-bucket",
		PlacementPolicyRef: "b2c_pooled_default",
	}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
}

// TestCMKProvisionAndRotate exercises the PUT /api/admin/cmk surface
// end-to-end: a freshly provisioned workspace defaults to the
// gateway-default key (empty cmk_uri), rotation persists a real
// AWS-KMS ARN, GET reflects the new value, an invalid scheme is
// rejected, and an empty PUT resets back to the default. The fabric
// console is intentionally not wired here so this test pins the
// purely-local persistence path; the upstream PutCMK call is
// covered separately once a fake console fixture lands.
func TestCMKProvisionAndRotate(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	wsID, err := uuid.Parse(tok.WorkspaceID)
	if err != nil {
		t.Fatalf("parse workspace id: %v", err)
	}
	seedFabricCredentials(t, env, wsID)

	// Default (gateway default) — empty string is the encoded form.
	status, body := env.httpRequest(http.MethodGet, "/api/admin/cmk", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get default cmk: status=%d body=%s", status, string(body))
	}
	var got struct {
		CMKURI string `json:"cmk_uri"`
	}
	env.decodeJSON(body, &got)
	if got.CMKURI != "" {
		t.Errorf("expected empty default cmk_uri, got %q", got.CMKURI)
	}

	// Rotate to an AWS KMS ARN.
	const arn = "arn:aws:kms:us-east-1:123456789012:key/abcdef01-2345-6789-abcd-ef0123456789"
	status, body = env.httpRequest(http.MethodPut, "/api/admin/cmk", tok.Token, map[string]string{
		"cmk_uri": arn,
	})
	if status != http.StatusOK {
		t.Fatalf("put cmk arn: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &got)
	if got.CMKURI != arn {
		t.Errorf("put echo: expected %q, got %q", arn, got.CMKURI)
	}

	// GET reflects the new value (and the row was actually persisted —
	// this is the "fabric provisioner would receive it" assertion).
	status, body = env.httpRequest(http.MethodGet, "/api/admin/cmk", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get rotated cmk: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &got)
	if got.CMKURI != arn {
		t.Errorf("get post-rotate: expected %q, got %q", arn, got.CMKURI)
	}

	// Invalid scheme is rejected with 400 — the row must not change.
	status, body = env.httpRequest(http.MethodPut, "/api/admin/cmk", tok.Token, map[string]string{
		"cmk_uri": "https://example.com/not-a-kms-uri",
	})
	if status != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid scheme, got %d body=%s", status, string(body))
	}
	status, body = env.httpRequest(http.MethodGet, "/api/admin/cmk", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get after-invalid: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &got)
	if got.CMKURI != arn {
		t.Errorf("invalid PUT must not mutate row; expected %q, got %q", arn, got.CMKURI)
	}

	// Each supported scheme is accepted.
	for _, uri := range []string{
		"kms://aws/alias/zk-drive",
		"vault://transit/zk-drive",
		"transit://kv/zk-drive",
	} {
		status, body = env.httpRequest(http.MethodPut, "/api/admin/cmk", tok.Token, map[string]string{
			"cmk_uri": uri,
		})
		if status != http.StatusOK {
			t.Errorf("put cmk %q: status=%d body=%s", uri, status, string(body))
		}
	}

	// Empty resets to gateway default.
	status, body = env.httpRequest(http.MethodPut, "/api/admin/cmk", tok.Token, map[string]string{
		"cmk_uri": "",
	})
	if status != http.StatusOK {
		t.Fatalf("reset cmk: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &got)
	if got.CMKURI != "" {
		t.Errorf("expected empty cmk_uri after reset, got %q", got.CMKURI)
	}

	// Round-trip the persisted value via the provisioner directly so
	// callers (the fabric tenant provisioning layer) can read what
	// the admin handler wrote.
	persisted, err := env.provisioner.LookupCMK(context.Background(), wsID)
	if err != nil {
		t.Fatalf("lookup persisted cmk: %v", err)
	}
	if persisted != "" {
		t.Errorf("provisioner sees stale cmk: %q", persisted)
	}
}

// TestCMKReturns404WhenWorkspaceNotProvisioned pins the contract
// that GET / PUT /api/admin/cmk respond 404 (rather than 500 or
// silently no-op) when the caller's workspace has no
// fabric-provisioned credentials row yet.
func TestCMKReturns404WhenWorkspaceNotProvisioned(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, _ := env.httpRequest(http.MethodGet, "/api/admin/cmk", tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("get: expected 404 when not provisioned, got %d", status)
	}
	status, _ = env.httpRequest(http.MethodPut, "/api/admin/cmk", tok.Token, map[string]string{
		"cmk_uri": "arn:aws:kms:us-east-1:123456789012:key/abc",
	})
	if status != http.StatusNotFound {
		t.Errorf("put: expected 404 when not provisioned, got %d", status)
	}
}
