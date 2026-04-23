package integration

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

func createFolder(t *testing.T, env *testEnv, token string, parentID *string, name string) folder.Folder {
	t.Helper()
	payload := map[string]any{"name": name}
	if parentID != nil {
		payload["parent_folder_id"] = *parentID
	}
	status, body := env.httpRequest(http.MethodPost, "/api/folders", token, payload)
	if status != http.StatusCreated {
		t.Fatalf("create folder: status=%d body=%s", status, string(body))
	}
	var f folder.Folder
	env.decodeJSON(body, &f)
	return f
}

func TestCreateRootFolder(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	f := createFolder(t, env, tok.Token, nil, "Engineering")
	if f.Name != "Engineering" {
		t.Errorf("name mismatch: %q", f.Name)
	}
	if f.Path != "/Engineering/" {
		t.Errorf("expected path=/Engineering/, got %q", f.Path)
	}
	if f.ParentFolderID != nil {
		t.Errorf("expected root folder to have nil parent, got %v", f.ParentFolderID)
	}
}

func TestCreateNestedFolder(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	parent := createFolder(t, env, tok.Token, nil, "Engineering")
	parentIDStr := parent.ID.String()
	child := createFolder(t, env, tok.Token, &parentIDStr, "Backend")

	if child.Path != "/Engineering/Backend/" {
		t.Errorf("expected nested path=/Engineering/Backend/, got %q", child.Path)
	}
	if child.ParentFolderID == nil || *child.ParentFolderID != parent.ID {
		t.Errorf("parent id mismatch: %v", child.ParentFolderID)
	}
}

func TestRenameFolder(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	f := createFolder(t, env, tok.Token, nil, "Engineering")
	status, body := env.httpRequest(http.MethodPut, "/api/folders/"+f.ID.String(), tok.Token, map[string]string{
		"name": "Platform",
	})
	if status != http.StatusOK {
		t.Fatalf("rename folder: status=%d body=%s", status, string(body))
	}
	var renamed folder.Folder
	env.decodeJSON(body, &renamed)
	if renamed.Name != "Platform" || renamed.Path != "/Platform/" {
		t.Errorf("unexpected rename result: %+v", renamed)
	}
}

func TestMoveFolder(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	src := createFolder(t, env, tok.Token, nil, "A")
	srcIDStr := src.ID.String()
	leaf := createFolder(t, env, tok.Token, &srcIDStr, "Leaf")
	dst := createFolder(t, env, tok.Token, nil, "B")
	dstIDStr := dst.ID.String()

	status, body := env.httpRequest(http.MethodPost, "/api/folders/"+src.ID.String()+"/move", tok.Token, map[string]any{
		"new_parent_folder_id": dstIDStr,
	})
	if status != http.StatusOK {
		t.Fatalf("move folder: status=%d body=%s", status, string(body))
	}
	var moved folder.Folder
	env.decodeJSON(body, &moved)
	if moved.Path != "/B/A/" {
		t.Errorf("expected moved path=/B/A/, got %q", moved.Path)
	}

	// Verify descendant path was rewritten.
	_, body = env.httpRequest(http.MethodGet, "/api/folders/"+leaf.ID.String(), tok.Token, nil)
	var wrap struct {
		Folder folder.Folder `json:"folder"`
	}
	env.decodeJSON(body, &wrap)
	if wrap.Folder.Path != "/B/A/Leaf/" {
		t.Errorf("descendant path not updated: %q", wrap.Folder.Path)
	}
}

func TestDeleteFolderSoftDeletesAndHidesFromListings(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	parent := createFolder(t, env, tok.Token, nil, "A")
	parentIDStr := parent.ID.String()
	createFolder(t, env, tok.Token, &parentIDStr, "Child")

	status, _ := env.httpRequest(http.MethodDelete, "/api/folders/"+parent.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete folder: status=%d", status)
	}

	status, _ = env.httpRequest(http.MethodGet, "/api/folders/"+parent.ID.String(), tok.Token, nil)
	if status != http.StatusNotFound {
		t.Errorf("expected 404 for deleted folder, got %d", status)
	}

	// Root listing should not include the deleted folder.
	status, body := env.httpRequest(http.MethodGet, "/api/folders?parent_folder_id=root", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list folders: %d", status)
	}
	var list struct {
		Folders []folder.Folder `json:"folders"`
	}
	env.decodeJSON(body, &list)
	for _, f := range list.Folders {
		if f.ID == parent.ID {
			t.Errorf("deleted folder still listed")
		}
	}
}

func TestListFolderChildren(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	parent := createFolder(t, env, tok.Token, nil, "A")
	parentIDStr := parent.ID.String()
	createFolder(t, env, tok.Token, &parentIDStr, "Child1")
	createFolder(t, env, tok.Token, &parentIDStr, "Child2")

	status, body := env.httpRequest(http.MethodGet, "/api/folders/"+parent.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get folder: %d", status)
	}
	var wrap struct {
		Children []folder.Folder `json:"children"`
	}
	env.decodeJSON(body, &wrap)
	if len(wrap.Children) != 2 {
		t.Errorf("expected 2 children, got %d", len(wrap.Children))
	}
}

func TestCannotCreateFolderInAnotherWorkspace(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "pw2")

	// Alice creates a folder in her workspace.
	aliceFolder := createFolder(t, env, alice.Token, nil, "Secret")
	aliceIDStr := aliceFolder.ID.String()

	// Bob tries to create a sub-folder under Alice's folder using his token
	// — should fail with invalid parent because the folder is not visible in
	// Bob's workspace.
	status, _ := env.httpRequest(http.MethodPost, "/api/folders", bob.Token, map[string]any{
		"name":             "Pwned",
		"parent_folder_id": aliceIDStr,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for cross-tenant folder creation, got %d", status)
	}

	// Bob cannot GET Alice's folder either.
	status, _ = env.httpRequest(http.MethodGet, "/api/folders/"+aliceIDStr, bob.Token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant folder read, got %d", status)
	}
}
