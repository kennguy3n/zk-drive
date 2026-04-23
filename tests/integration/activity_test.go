package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/kennguy3n/zk-drive/internal/activity"
)

// waitForActivity polls the activity_log table until at least `n` rows
// exist for the given workspace or the timeout elapses. Activity logging
// is fire-and-forget via a background goroutine, so tests can't assume the
// row is visible immediately after the HTTP response returns.
func waitForActivity(t *testing.T, env *testEnv, workspaceID string, token string, n int) []activityEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, body := env.httpRequest(http.MethodGet, "/api/activity?limit=200", token, nil)
		if status != http.StatusOK {
			t.Fatalf("list activity: status=%d body=%s", status, string(body))
		}
		var wrap struct {
			Entries []activityEntry `json:"entries"`
		}
		env.decodeJSON(body, &wrap)
		if len(wrap.Entries) >= n {
			return wrap.Entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for >= %d activity entries, got %d", n, len(wrap.Entries))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// activityEntry mirrors the fields we assert on. Defined here instead of
// reusing activity.LogEntry because the JSON tags include raw metadata and
// re-parsing that is not useful for these tests.
type activityEntry struct {
	ID           string `json:"id"`
	WorkspaceID  string `json:"workspace_id"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

func TestActivityLoggedForFolderCreate(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	f := createFolder(t, env, tok.Token, nil, "Engineering")

	entries := waitForActivity(t, env, tok.WorkspaceID, tok.Token, 1)
	found := false
	for _, e := range entries {
		if e.Action == activity.ActionFolderCreate && e.ResourceID == f.ID.String() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("folder.create activity not logged; entries=%+v", entries)
	}
}

func TestActivityLoggedForFileUpload(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	// CreateFile requires a folder_id — a missing or empty one 400s
	// before the activity hook fires. Create a parent folder so the
	// file create actually succeeds.
	parent := createFolder(t, env, tok.Token, nil, "Inbox")

	status, body := env.httpRequest(http.MethodPost, "/api/files", tok.Token, map[string]any{
		"name":      "notes.txt",
		"folder_id": parent.ID.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("create file: status=%d body=%s", status, string(body))
	}
	var file struct {
		ID string `json:"id"`
	}
	env.decodeJSON(body, &file)

	// The folder create above also emits an activity entry, so we wait
	// for at least 2 entries before asserting the file.create is there.
	entries := waitForActivity(t, env, tok.WorkspaceID, tok.Token, 2)
	found := false
	for _, e := range entries {
		if e.Action == activity.ActionFileCreate && e.ResourceID == file.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("file.create activity not logged; entries=%+v", entries)
	}
}

func TestActivityPagination(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pass")

	// Generate five distinct activity events.
	for i := 0; i < 5; i++ {
		createFolder(t, env, tok.Token, nil, "F"+string(rune('A'+i)))
	}
	waitForActivity(t, env, tok.WorkspaceID, tok.Token, 5)

	// First page: limit=2.
	status, body := env.httpRequest(http.MethodGet, "/api/activity?limit=2&offset=0", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("page 1: status=%d", status)
	}
	var page1 struct {
		Entries []activityEntry `json:"entries"`
	}
	env.decodeJSON(body, &page1)
	if len(page1.Entries) != 2 {
		t.Fatalf("page 1 expected 2 entries, got %d", len(page1.Entries))
	}

	// Second page should not overlap.
	status, body = env.httpRequest(http.MethodGet, "/api/activity?limit=2&offset=2", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("page 2: status=%d", status)
	}
	var page2 struct {
		Entries []activityEntry `json:"entries"`
	}
	env.decodeJSON(body, &page2)
	if len(page2.Entries) != 2 {
		t.Fatalf("page 2 expected 2 entries, got %d", len(page2.Entries))
	}
	for _, a := range page1.Entries {
		for _, b := range page2.Entries {
			if a.ID == b.ID {
				t.Errorf("pagination overlap: %s appears on both pages", a.ID)
			}
		}
	}
}

func TestActivityCrossTenantIsolation(t *testing.T) {
	env := setupEnv(t)
	alice := env.signupAndLogin("Acme", "alice@acme.test", "Alice", "pw1")
	bob := env.signupAndLogin("Globex", "bob@globex.test", "Bob", "pw2")

	createFolder(t, env, alice.Token, nil, "AliceOnly")
	waitForActivity(t, env, alice.WorkspaceID, alice.Token, 1)

	// Bob's activity feed must not contain Alice's events.
	status, body := env.httpRequest(http.MethodGet, "/api/activity?limit=200", bob.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("bob list: status=%d", status)
	}
	var wrap struct {
		Entries []activityEntry `json:"entries"`
	}
	env.decodeJSON(body, &wrap)
	for _, e := range wrap.Entries {
		if e.WorkspaceID == alice.WorkspaceID {
			t.Fatalf("bob saw alice's activity entry: %+v", e)
		}
	}

	// workspace_id mismatch param must be rejected.
	status, _ = env.httpRequest(http.MethodGet,
		"/api/activity?workspace_id="+alice.WorkspaceID, bob.Token, nil)
	if status != http.StatusForbidden {
		t.Errorf("expected 403 for workspace_id mismatch, got %d", status)
	}
}
