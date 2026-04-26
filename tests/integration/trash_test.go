package integration

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestSoftDeleteAndRestore(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "memo.txt", "text/plain")

	status, _ := env.httpRequest(http.MethodDelete, "/api/files/"+f.ID.String(), tok.Token, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", status)
	}

	// GET no longer surfaces the file (handler filters deleted_at).
	status, _ = env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String(), tok.Token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for deleted file, got %d", status)
	}

	// Soft-delete preserves the row; the restore-from-trash flow is
	// not yet exposed via HTTP so we assert the invariant directly:
	// row still exists with deleted_at set.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var deletedAt *time.Time
	if err := env.pool.QueryRow(ctx, `SELECT deleted_at FROM files WHERE id = $1`, f.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("expected deleted_at to be set after DELETE")
	}

	// Manually clear deleted_at to simulate a restore; afterwards the
	// row must surface again through the public API. This pins down
	// the soft-delete semantic the future restore endpoint will rely on.
	if _, err := env.pool.Exec(ctx, `UPDATE files SET deleted_at = NULL WHERE id = $1`, f.ID); err != nil {
		t.Fatalf("restore (simulated): %v", err)
	}
	status, _ = env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String(), tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200 after restore, got %d", status)
	}
}
