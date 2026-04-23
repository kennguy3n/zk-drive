package integration

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
)

func createFile(t *testing.T, env *testEnv, token string, folderID, name, mime string) file.File {
	t.Helper()
	status, body := env.httpRequest(http.MethodPost, "/api/files", token, map[string]string{
		"folder_id": folderID,
		"name":      name,
		"mime_type": mime,
	})
	if status != http.StatusCreated {
		t.Fatalf("create file: status=%d body=%s", status, string(body))
	}
	var f file.File
	env.decodeJSON(body, &f)
	return f
}

func TestCreateFileMetadata(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	f := createFile(t, env, tok.Token, fold.ID.String(), "report.pdf", "application/pdf")
	if f.Name != "report.pdf" || f.MimeType != "application/pdf" {
		t.Errorf("create file mismatch: %+v", f)
	}
	if f.FolderID != fold.ID {
		t.Errorf("folder id mismatch")
	}
}

func TestGetFileByID(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "report.pdf", "application/pdf")

	status, body := env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get file: status=%d body=%s", status, string(body))
	}
	var got file.File
	env.decodeJSON(body, &got)
	if got.ID != f.ID {
		t.Errorf("id mismatch")
	}
}

func TestRenameFile(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "report.pdf", "application/pdf")

	status, body := env.httpRequest(http.MethodPut, "/api/files/"+f.ID.String(), tok.Token, map[string]string{
		"name": "final.pdf",
	})
	if status != http.StatusOK {
		t.Fatalf("rename file: status=%d body=%s", status, string(body))
	}
	var renamed file.File
	env.decodeJSON(body, &renamed)
	if renamed.Name != "final.pdf" {
		t.Errorf("rename failed: %q", renamed.Name)
	}
}

func TestMoveFile(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	src := createFolder(t, env, tok.Token, nil, "Inbox")
	dst := createFolder(t, env, tok.Token, nil, "Archive")
	f := createFile(t, env, tok.Token, src.ID.String(), "note.txt", "text/plain")

	status, body := env.httpRequest(http.MethodPost, "/api/files/"+f.ID.String()+"/move", tok.Token, map[string]string{
		"folder_id": dst.ID.String(),
	})
	if status != http.StatusOK {
		t.Fatalf("move file: status=%d body=%s", status, string(body))
	}
	var moved file.File
	env.decodeJSON(body, &moved)
	if moved.FolderID != dst.ID {
		t.Errorf("move did not update folder")
	}
}

func TestDeleteFileSoftDeletes(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "note.txt", "text/plain")

	status, _ := env.httpRequest(http.MethodDelete, "/api/files/"+f.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete file: status=%d", status)
	}
	status, _ = env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String(), tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", status)
	}
}

func TestDeletedFileNotInListings(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "a.txt", "text/plain")
	_ = createFile(t, env, tok.Token, fold.ID.String(), "b.txt", "text/plain")

	status, _ := env.httpRequest(http.MethodDelete, "/api/files/"+f.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete file: status=%d", status)
	}

	status, body := env.httpRequest(http.MethodGet, "/api/folders/"+fold.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get folder: status=%d", status)
	}
	var wrap struct {
		Folder folder.Folder `json:"folder"`
		Files  []file.File   `json:"files"`
	}
	env.decodeJSON(body, &wrap)
	if len(wrap.Files) != 1 || wrap.Files[0].Name != "b.txt" {
		t.Errorf("unexpected files after delete: %+v", wrap.Files)
	}
}

func TestCannotAccessFileInAnotherWorkspace(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "pw2")

	aliceFolder := createFolder(t, env, alice.Token, nil, "Docs")
	aliceFile := createFile(t, env, alice.Token, aliceFolder.ID.String(), "secret.txt", "text/plain")

	status, _ := env.httpRequest(http.MethodGet, "/api/files/"+aliceFile.ID.String(), bob.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for cross-tenant file read, got %d", status)
	}

	// Bob also cannot rename or delete it.
	status, _ = env.httpRequest(http.MethodPut, "/api/files/"+aliceFile.ID.String(), bob.Token, map[string]string{
		"name": "pwned.txt",
	})
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for cross-tenant file rename, got %d", status)
	}
}
