package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// TestSearchExcludesStrictZKFiles asserts that the FTS query never
// surfaces files (or folders) whose parent folder is in strict-ZK
// mode. Strict-ZK plaintext stays out of the server's index by design;
// the previous query joined folders but did not filter on
// encryption_mode, leaking strict-ZK file names into search results.
func TestSearchExcludesStrictZKFiles(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Create one strict-ZK folder and one managed-encrypted folder.
	zkPayload := map[string]string{"name": "Vault", "encryption_mode": folder.EncryptionStrictZK}
	status, body := env.httpRequest(http.MethodPost, "/api/folders", tok.Token, zkPayload)
	if status != http.StatusCreated {
		t.Fatalf("create strict-zk folder: status=%d body=%s", status, string(body))
	}
	var zkFolder folder.Folder
	env.decodeJSON(body, &zkFolder)
	if zkFolder.EncryptionMode != folder.EncryptionStrictZK {
		t.Fatalf("expected strict_zk mode, got %q", zkFolder.EncryptionMode)
	}
	regular := createFolder(t, env, tok.Token, nil, "Public")

	const distinctive = "topsecretdocname"

	// File in the strict-ZK folder must not surface in search. The
	// 'simple' tsvector parser treats "name.ext" as one token, so we
	// keep the distinctive token as a standalone word with the
	// extension separated by a space.
	zkFile := createFile(t, env, tok.Token, zkFolder.ID.String(), distinctive+" report", "text/plain")
	status, body = env.httpRequest(http.MethodGet, "/api/search?q="+distinctive, tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search 1: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Hits []struct {
			ID   uuid.UUID `json:"id"`
			Type string    `json:"type"`
		} `json:"hits"`
	}
	env.decodeJSON(body, &resp)
	for _, h := range resp.Hits {
		if h.ID == zkFile.ID {
			t.Fatalf("strict-ZK file leaked into search results: %+v", resp.Hits)
		}
	}

	// Same name under a regular (non-strict-ZK) folder must surface.
	regFile := createFile(t, env, tok.Token, regular.ID.String(), distinctive+" report", "text/plain")
	status, body = env.httpRequest(http.MethodGet, "/api/search?q="+distinctive, tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search 2: status=%d body=%s", status, string(body))
	}
	env.decodeJSON(body, &resp)
	found := false
	for _, h := range resp.Hits {
		if h.ID == regFile.ID {
			found = true
		}
		if h.ID == zkFile.ID {
			t.Fatalf("strict-ZK file leaked on second search: %+v", resp.Hits)
		}
	}
	if !found {
		t.Fatalf("expected non-strict-ZK file %s in results, hits=%+v", regFile.ID, resp.Hits)
	}
}
