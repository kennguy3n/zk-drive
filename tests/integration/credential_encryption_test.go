package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
	"github.com/kennguy3n/zk-drive/internal/fabric"
)

// TestProvisionerPersistsEncryptedSecret asserts that the AES-GCM
// codec wired through the fabric provisioner stores the secret_key
// column as ciphertext (not plaintext), and that the storage
// factory's decryptor recovers the original value.
func TestProvisionerPersistsEncryptedSecret(t *testing.T) {
	env := setupEnv(t)

	// Sign up to land a workspace row that the FK on
	// workspace_storage_credentials can reference.
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	wsID, err := uuid.Parse(tok.WorkspaceID)
	if err != nil {
		t.Fatalf("parse workspace id: %v", err)
	}

	codec, err := cryptopkg.NewAESGCMCodec(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}

	provisioner := fabric.NewProvisioner(env.pool, fabric.Config{
		ConsoleURL:       "https://fabric.test",
		BucketTemplate:   "zk-drive-{tenant}",
		DefaultPolicyRef: "b2c_pooled_default",
		Encryptor:        codec,
	})

	const plaintextSecret = "super-secret-fabric-key"
	creds := &fabric.Credentials{
		WorkspaceID:        wsID,
		TenantID:           "tenant-test",
		AccessKey:          "AKIA-test",
		SecretKey:          plaintextSecret,
		Endpoint:           "https://fabric.test",
		Bucket:             "zk-drive-tenant-test",
		PlacementPolicyRef: "b2c_pooled_default",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := provisioner.Persist(ctx, creds); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var stored string
	if err := env.pool.QueryRow(ctx,
		`SELECT secret_key_encrypted FROM workspace_storage_credentials WHERE workspace_id = $1`,
		wsID).Scan(&stored); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if stored == plaintextSecret {
		t.Fatal("stored secret matches plaintext; codec must be applied")
	}
	if !strings.HasPrefix(stored, "aesgcm:") {
		t.Fatalf("expected aesgcm: prefix on stored secret, got %q", stored)
	}

	// Round-trip: the decryptor recovers the plaintext.
	got, err := codec.Decrypt(ctx, stored)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plaintextSecret {
		t.Fatalf("decrypted secret mismatch: got %q, want %q", got, plaintextSecret)
	}
}
