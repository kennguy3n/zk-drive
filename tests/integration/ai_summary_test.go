package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// TestThreadSummaryRespectsEncryptionMode exercises the AI summary
// scaffold end-to-end through POST /api/kchat/rooms/{id}/summary.
// Managed folders return a deterministic non-empty summary; strict-ZK
// folders refuse with 403 so the server never pretends to produce a
// summary over plaintext it does not have.
func TestThreadSummaryRespectsEncryptionMode(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Managed room: create mapping + a file, then expect 200.
	const managedRoom = "kchat-room-managed"
	status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": managedRoom,
	})
	if status != http.StatusCreated {
		t.Fatalf("create managed room: status=%d body=%s", status, string(body))
	}
	var managed kchatRoomCreated
	env.decodeJSON(body, &managed)

	// Uploading a file forces content indexing on a real deployment
	// but for the scaffold test a plain metadata row is enough; the
	// summary renders the file name regardless of content_text.
	createFile(t, env, tok.Token, managed.FolderID.String(), "notes.txt", "text/plain")

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms/"+managed.ID.String()+"/summary", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("managed summary: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode summary body: %v", err)
	}
	if strings.TrimSpace(resp.Summary) == "" {
		t.Fatalf("expected non-empty summary, got %q", resp.Summary)
	}
	if !strings.Contains(resp.Summary, "notes.txt") {
		t.Errorf("summary did not mention uploaded file: %q", resp.Summary)
	}

	// Strict-ZK room: create mapping, flip the folder into strict_zk
	// out-of-band (there is no HTTP API to mutate encryption_mode
	// today — this matches the search_strictzk_test pattern), then
	// expect a 403 from /summary.
	const strictRoom = "kchat-room-strict"
	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms", tok.Token, map[string]string{
		"kchat_room_id": strictRoom,
	})
	if status != http.StatusCreated {
		t.Fatalf("create strict room: status=%d body=%s", status, string(body))
	}
	var strict kchatRoomCreated
	env.decodeJSON(body, &strict)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := env.pool.Exec(ctx,
		`UPDATE folders SET encryption_mode = $1 WHERE id = $2`,
		folder.EncryptionStrictZK, strict.FolderID); err != nil {
		t.Fatalf("flip folder to strict_zk: %v", err)
	}

	status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms/"+strict.ID.String()+"/summary", tok.Token, nil)
	if status != http.StatusForbidden {
		t.Fatalf("strict summary: expected 403, got status=%d body=%s", status, string(body))
	}
}
