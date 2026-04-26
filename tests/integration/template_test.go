package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/sharing"
)

// TestClientRoomTemplateCreate exercises POST
// /api/client-rooms/from-template for the agency vertical: a fresh
// room is created with the four agency sub-folders, and they appear
// as children under the room's root folder.
func TestClientRoomTemplateCreate(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	status, body := env.httpRequest(http.MethodPost, "/api/client-rooms/from-template", tok.Token,
		map[string]any{
			"name":     "Globex Brand Refresh",
			"template": "agency",
		})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, string(body))
	}
	var resp struct {
		ID             uuid.UUID   `json:"id"`
		FolderID       uuid.UUID   `json:"folder_id"`
		Name           string      `json:"name"`
		ShareLinkToken string      `json:"share_link_token"`
		SubFolderIDs   []uuid.UUID `json:"sub_folder_ids"`
	}
	env.decodeJSON(body, &resp)
	if resp.ID == uuid.Nil || resp.FolderID == uuid.Nil {
		t.Fatalf("response missing ids: %+v", resp)
	}
	want := []string{"Briefs", "Assets", "Deliverables", "Feedback"}
	if len(resp.SubFolderIDs) != len(want) {
		t.Fatalf("sub_folder_ids: expected %d, got %d", len(want), len(resp.SubFolderIDs))
	}

	// Each sub-folder is in fact a child of the room folder, with the
	// expected name and order.
	status, body = env.httpRequest(http.MethodGet, "/api/folders/"+resp.FolderID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("get room folder: status=%d body=%s", status, string(body))
	}
	var folderResp struct {
		Children []folder.Folder `json:"children"`
	}
	env.decodeJSON(body, &folderResp)
	got := childNames(folderResp.Children)
	if !sameSet(got, want) {
		t.Errorf("sub-folders: got %v, want %v (any order)", got, want)
	}
	for _, c := range folderResp.Children {
		if c.WorkspaceID.String() != tok.WorkspaceID {
			t.Errorf("sub-folder %s in wrong workspace: %s", c.Name, c.WorkspaceID)
		}
	}

	// The legal template is also resolvable; this guards against a
	// drift where someone accidentally drops a vertical from the
	// builtinTemplates registry.
	status, body = env.httpRequest(http.MethodPost, "/api/client-rooms/from-template", tok.Token,
		map[string]any{
			"name":     "Doe v. Smith",
			"template": "legal",
		})
	if status != http.StatusCreated {
		t.Fatalf("create legal: status=%d body=%s", status, string(body))
	}
	var legal struct {
		SubFolderIDs []uuid.UUID `json:"sub_folder_ids"`
	}
	env.decodeJSON(body, &legal)
	if len(legal.SubFolderIDs) != 4 {
		t.Errorf("legal: expected 4 sub-folders, got %d", len(legal.SubFolderIDs))
	}

	// Unknown templates 400, not 500.
	status, _ = env.httpRequest(http.MethodPost, "/api/client-rooms/from-template", tok.Token,
		map[string]any{
			"name":     "Bogus",
			"template": "no-such-template",
		})
	if status != http.StatusBadRequest {
		t.Errorf("unknown template: expected 400, got %d", status)
	}

	// GET /templates surfaces every built-in.
	status, body = env.httpRequest(http.MethodGet, "/api/client-rooms/templates", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("list templates: status=%d body=%s", status, string(body))
	}
	var tplResp struct {
		Templates []struct {
			Name       string   `json:"name"`
			SubFolders []string `json:"sub_folders"`
		} `json:"templates"`
	}
	env.decodeJSON(body, &tplResp)
	if len(tplResp.Templates) != len(sharing.ListTemplates()) {
		t.Errorf("templates: API returns %d, registry has %d", len(tplResp.Templates), len(sharing.ListTemplates()))
	}

	// Order is part of the contract: ListTemplates sorts by Name
	// ascending, so the response is identical across calls. A future
	// caller that re-introduces map-iteration order would flake this.
	wantOrder := []string{"accounting", "agency", "clinic", "construction", "legal"}
	if len(tplResp.Templates) != len(wantOrder) {
		t.Fatalf("templates: expected %d entries, got %d", len(wantOrder), len(tplResp.Templates))
	}
	for i, want := range wantOrder {
		if tplResp.Templates[i].Name != want {
			t.Errorf("templates[%d]: expected %q, got %q", i, want, tplResp.Templates[i].Name)
		}
	}
}

func childNames(cs []folder.Folder) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
