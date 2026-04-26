package integration

import (
	"net/http"
	"testing"
)

func TestBulkMoveCrossWorkspaceRejected(t *testing.T) {
	env := setupEnv(t)

	// Workspace A: create the source files.
	tokA := env.signupAndLogin("Acme A", "admin@acme-a.test", "Alice", "pw")
	folderA := createFolder(t, env, tokA.Token, nil, "DocsA")
	srcA := createFile(t, env, tokA.Token, folderA.ID.String(), "shared.txt", "text/plain")

	// Workspace B: create a target folder. Tenant guard ensures
	// tokA cannot reach this folder by id.
	tokB := env.signupAndLogin("Acme B", "admin@acme-b.test", "Bob", "pw")
	folderB := createFolder(t, env, tokB.Token, nil, "DocsB")

	status, body := env.httpRequest(http.MethodPost, "/api/bulk/move", tokA.Token, map[string]any{
		"file_ids":         []string{srcA.ID.String()},
		"target_folder_id": folderB.ID.String(),
	})
	// Cross-workspace target lookup misses inside workspace A so the
	// handler returns 404 before any file is moved.
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 cross-workspace, got %d body=%s", status, string(body))
	}

	// Sanity: srcA still resolves under workspace A (i.e. it
	// wasn't moved despite the failed bulk attempt).
	status, _ = env.httpRequest(http.MethodGet, "/api/files/"+srcA.ID.String(), tokA.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("expected source file still readable in A, got %d", status)
	}
	_ = body
}

func TestBulkDeleteSoftDeletes(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	a := createFile(t, env, tok.Token, fold.ID.String(), "a.txt", "text/plain")
	b := createFile(t, env, tok.Token, fold.ID.String(), "b.txt", "text/plain")

	status, body := env.httpRequest(http.MethodPost, "/api/bulk/delete", tok.Token, map[string]any{
		"file_ids": []string{a.ID.String(), b.ID.String()},
	})
	if status != http.StatusOK {
		t.Fatalf("bulk delete: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Succeeded []string `json:"succeeded"`
		Failed    []any    `json:"failed"`
	}
	env.decodeJSON(body, &resp)
	if len(resp.Succeeded) != 2 || len(resp.Failed) != 0 {
		t.Fatalf("expected 2 succeeded / 0 failed, got %+v", resp)
	}

	for _, fid := range []string{a.ID.String(), b.ID.String()} {
		status, _ := env.httpRequest(http.MethodGet, "/api/files/"+fid, tok.Token, nil)
		if status != http.StatusNotFound {
			t.Errorf("expected 404 for soft-deleted file %s, got %d", fid, status)
		}
	}
}
