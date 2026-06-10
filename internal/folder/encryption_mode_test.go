package folder_test

import (
	"testing"

	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// TestEncryptionModeConstantsInSync guards against drift between the
// folder package's canonical encryption-mode constants and the
// workspace package's re-declaration of them (the workspace package
// can't import folder without an import cycle, so it mirrors the
// string values). The two MUST stay byte-identical: the
// workspaces.default_encryption_mode column feeds straight into
// folder.Service's root-folder default, and a mismatch would silently
// produce folders whose mode the folder layer rejects.
func TestEncryptionModeConstantsInSync(t *testing.T) {
	if folder.EncryptionManagedEncrypted != workspace.EncryptionManagedEncrypted {
		t.Fatalf("managed_encrypted constant drift: folder=%q workspace=%q",
			folder.EncryptionManagedEncrypted, workspace.EncryptionManagedEncrypted)
	}
	if folder.EncryptionStrictZK != workspace.EncryptionStrictZK {
		t.Fatalf("strict_zk constant drift: folder=%q workspace=%q",
			folder.EncryptionStrictZK, workspace.EncryptionStrictZK)
	}
	// The workspace default must itself be a mode the folder layer
	// accepts, or every root-folder create under the default would
	// fail validation.
	if !folder.IsValidEncryptionMode(workspace.DefaultEncryptionMode) {
		t.Fatalf("workspace.DefaultEncryptionMode %q is not a valid folder encryption mode",
			workspace.DefaultEncryptionMode)
	}
}
