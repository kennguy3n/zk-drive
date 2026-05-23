package integration

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/webhooks"
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

	// Webhook contract: BulkDelete MUST emit one file.deleted event
	// per soft-deleted file so subscribers see the same event stream
	// regardless of whether the admin used the single-file DELETE
	// /api/files/{id} endpoint or the batch /api/bulk/delete
	// endpoint. Without this assertion the gap is silent — the bulk
	// path soft-deletes the rows but webhook subscribers never see
	// the events, which is exactly the consistency bug reviewed in
	// PR #69 (Devin Review finding bulk.go:138).
	deletedEvents := env.webhooks.fileEventsByType(webhooks.EventFileDeleted)
	if len(deletedEvents) != 2 {
		t.Fatalf("expected 2 file.deleted webhook events for bulk delete, got %d", len(deletedEvents))
	}
	gotIDs := map[string]bool{}
	for _, e := range deletedEvents {
		gotIDs[e.Data.FileID.String()] = true
		if e.Data.Name == "" {
			t.Errorf("expected webhook payload to carry pre-delete name snapshot, got empty for file %s", e.Data.FileID)
		}
	}
	for _, want := range []string{a.ID.String(), b.ID.String()} {
		if !gotIDs[want] {
			t.Errorf("expected file.deleted webhook for %s, got events %+v", want, deletedEvents)
		}
	}
}

// TestBulkDeleteFolderCascadesFileWebhooks pins the round-8
// consistency fix: when a folder containing files (and nested
// sub-folders containing files) is bulk-deleted, the
// folder.SoftDeleteSubtree cascade soft-deletes every file under the
// subtree in one transaction. Webhook subscribers must see a
// file.deleted event per cascaded file — without this they have no
// way to distinguish "the folder still exists" from "the folder and
// every file underneath are gone", which breaks audit/sync
// downstream workflows.
func TestBulkDeleteFolderCascadesFileWebhooks(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Two-level tree:
	//   parent/
	//     top.txt
	//     child/
	//       nested.txt
	parent := createFolder(t, env, tok.Token, nil, "parent")
	top := createFile(t, env, tok.Token, parent.ID.String(), "top.txt", "text/plain")
	childID := parent.ID.String()
	child := createFolder(t, env, tok.Token, &childID, "child")
	nested := createFile(t, env, tok.Token, child.ID.String(), "nested.txt", "text/plain")

	// fileEventsByType filters captured events by type so the
	// file.upload.confirmed events from createFile setup don't
	// pollute the file.deleted assertion below.
	status, _ := env.httpRequest(http.MethodPost, "/api/bulk/delete", tok.Token, map[string]any{
		"folder_ids": []string{parent.ID.String()},
	})
	if status != http.StatusOK {
		t.Fatalf("bulk delete folder: status=%d", status)
	}

	deletedEvents := env.webhooks.fileEventsByType(webhooks.EventFileDeleted)
	if len(deletedEvents) != 2 {
		t.Fatalf("expected 2 file.deleted webhook events for folder cascade (top.txt + nested.txt), got %d: %+v", len(deletedEvents), deletedEvents)
	}
	gotIDs := map[string]bool{}
	gotNames := map[string]string{}
	for _, e := range deletedEvents {
		gotIDs[e.Data.FileID.String()] = true
		gotNames[e.Data.FileID.String()] = e.Data.Name
	}
	for _, want := range []struct{ id, name string }{
		{top.ID.String(), "top.txt"},
		{nested.ID.String(), "nested.txt"},
	} {
		if !gotIDs[want.id] {
			t.Errorf("expected file.deleted webhook for cascaded file %s (%s), got events %+v", want.id, want.name, deletedEvents)
		}
		if gotNames[want.id] != want.name {
			t.Errorf("expected pre-delete name snapshot %q for file %s, got %q", want.name, want.id, gotNames[want.id])
		}
	}
}

// TestDeleteFolderSingleCascadesFileWebhooks is the matching
// single-folder DELETE /api/folders/{id} test — same contract as
// the bulk version above. Captured here in this file because it
// uses the same helpers and the two paths share the cascade
// machinery in api/drive/webhook_events.go.
func TestDeleteFolderSingleCascadesFileWebhooks(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	parent := createFolder(t, env, tok.Token, nil, "parent")
	inside := createFile(t, env, tok.Token, parent.ID.String(), "inside.txt", "text/plain")

	status, _ := env.httpRequest(http.MethodDelete, "/api/folders/"+parent.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete folder: status=%d", status)
	}

	deletedEvents := env.webhooks.fileEventsByType(webhooks.EventFileDeleted)
	if len(deletedEvents) != 1 {
		t.Fatalf("expected 1 file.deleted webhook for single-folder cascade, got %d", len(deletedEvents))
	}
	if deletedEvents[0].Data.FileID != inside.ID {
		t.Errorf("file.deleted FileID: got=%s want=%s", deletedEvents[0].Data.FileID, inside.ID)
	}
	if deletedEvents[0].Data.Name != "inside.txt" {
		t.Errorf("file.deleted Name: got=%q want=inside.txt", deletedEvents[0].Data.Name)
	}
}
