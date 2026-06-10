package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// defaultEncModeResp mirrors the admin endpoint's JSON contract.
type defaultEncModeResp struct {
	Mode      string   `json:"mode"`
	Supported []string `json:"supported"`
}

// TestAdminDefaultEncryptionMode_GetAndSet ensures the GET endpoint
// exposes the supported allow-list and the default (managed), and the
// PUT endpoint persists a switch to strict_zk.
func TestAdminDefaultEncryptionMode_GetAndSet(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodGet, "/api/admin/workspace/default-encryption-mode", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get default encryption mode: status=%d body=%s", status, string(body))
	}
	var resp defaultEncModeResp
	env.decodeJSON(body, &resp)
	if resp.Mode != workspace.EncryptionManagedEncrypted {
		t.Errorf("expected default mode=%q, got %q", workspace.EncryptionManagedEncrypted, resp.Mode)
	}
	foundStrict := false
	for _, m := range resp.Supported {
		if m == workspace.EncryptionStrictZK {
			foundStrict = true
		}
	}
	if !foundStrict {
		t.Errorf("expected strict_zk in supported list, got %v", resp.Supported)
	}

	// Flip to strict_zk.
	status, body = env.httpRequest(http.MethodPut, "/api/admin/workspace/default-encryption-mode", tok.Token, map[string]string{
		"mode": workspace.EncryptionStrictZK,
	})
	if status != http.StatusOK {
		t.Fatalf("set strict_zk: status=%d body=%s", status, string(body))
	}
	// Confirm it stuck.
	status, body = env.httpRequest(http.MethodGet, "/api/admin/workspace/default-encryption-mode", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get after set: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &resp)
	if resp.Mode != workspace.EncryptionStrictZK {
		t.Errorf("expected mode=strict_zk after set, got %q", resp.Mode)
	}
}

// TestAdminDefaultEncryptionMode_Validation pins the 400 contract: an
// unsupported value must be rejected before it can violate the
// migration's CHECK constraint, and a missing key must not silently
// no-op.
func TestAdminDefaultEncryptionMode_Validation(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodPut, "/api/admin/workspace/default-encryption-mode", tok.Token, map[string]string{
		"mode": "rot13",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported mode, got %d body=%s", status, string(body))
	}
	if !strings.Contains(strings.ToLower(string(body)), "encryption mode") {
		t.Errorf("expected response to mention encryption mode, got %s", string(body))
	}

	// Missing key also rejected (pointer field distinguishes absent).
	status, _ = env.httpRequest(http.MethodPut, "/api/admin/workspace/default-encryption-mode", tok.Token, map[string]string{})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing mode key, got %d", status)
	}
}

// TestDefaultEncryptionMode_AppliesToNewRootFolder is the end-to-end
// guarantee of 6.4: once an admin sets the workspace default to
// strict_zk, a NEW root folder created without an explicit mode is
// strict zero-knowledge — while a managed-encrypted default yields a
// managed-encrypted root folder. Child folders always inherit their
// parent regardless of the workspace default.
func TestDefaultEncryptionMode_AppliesToNewRootFolder(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Baseline: default is managed-encrypted, so a root folder is too.
	managed := createFolder(t, env, tok.Token, nil, "Managed")
	if managed.EncryptionMode != folder.EncryptionManagedEncrypted {
		t.Fatalf("baseline root mode = %q, want %q", managed.EncryptionMode, folder.EncryptionManagedEncrypted)
	}

	// Flip the workspace default to strict_zk.
	status, body := env.httpRequest(http.MethodPut, "/api/admin/workspace/default-encryption-mode", tok.Token, map[string]string{
		"mode": workspace.EncryptionStrictZK,
	})
	if status != http.StatusOK {
		t.Fatalf("set strict_zk default: status=%d body=%s", status, string(body))
	}

	// A new root folder (no explicit mode) must now be strict_zk.
	strict := createFolder(t, env, tok.Token, nil, "Secret")
	if strict.EncryptionMode != folder.EncryptionStrictZK {
		t.Fatalf("root mode after strict default = %q, want %q", strict.EncryptionMode, folder.EncryptionStrictZK)
	}

	// A child of the strict root inherits strict (parent inheritance),
	// confirming the workspace default governs roots, not the override
	// of explicit inheritance.
	strictIDStr := strict.ID.String()
	child := createFolder(t, env, tok.Token, &strictIDStr, "Child")
	if child.EncryptionMode != folder.EncryptionStrictZK {
		t.Fatalf("child mode = %q, want inherited %q", child.EncryptionMode, folder.EncryptionStrictZK)
	}
}
