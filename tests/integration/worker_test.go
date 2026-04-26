package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/index"
)

// TestIndexWorkerExtractsText asserts that text the worker extracts
// from an uploaded file lands in files.content_text and is then
// reachable via the FTS query in /api/search. It bypasses the
// presigned-URL download by calling index.Service.PersistContent
// directly so the test runs without a live storage gateway.
//
// It also exercises ExtractText to confirm the worker only writes
// content for supported mime types (text/* + json/xml today).
func TestIndexWorkerExtractsText(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")

	const distinctive = "blueberrysorbet"
	body := []byte("internal memo body mentions " + distinctive + " in the second paragraph.")

	fileID := confirmUploadHelper(t, env, tok.Token, fold.ID, "memo.txt", "text/plain", int64(len(body)))

	// Sanity: ExtractText returns the body verbatim for text/plain.
	got, err := index.ExtractText("text/plain", body)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}
	if !strings.Contains(got, distinctive) {
		t.Fatalf("extracted text missing token; got=%q", got)
	}

	// PersistContent is what the worker calls after a successful
	// download + extract. We call it directly so this test can run
	// without a live zk-object-fabric gateway.
	svc := index.NewService(env.pool, env.storage, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.PersistContent(ctx, fileID, got); err != nil {
		t.Fatalf("persist content: %v", err)
	}

	// Now /api/search?q=<token in body, not name> should surface this
	// file by way of files.content_text.
	status, raw := env.httpRequest(http.MethodGet, "/api/search?q="+distinctive, tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(raw))
	}
	var resp struct {
		Hits []struct {
			ID   uuid.UUID `json:"id"`
			Type string    `json:"type"`
			Name string    `json:"name"`
		} `json:"hits"`
	}
	env.decodeJSON(raw, &resp)
	found := false
	for _, h := range resp.Hits {
		if h.ID == fileID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected file %s in search hits for token %q; hits=%+v", fileID, distinctive, resp.Hits)
	}

	// Unsupported types must return ErrUnsupportedMimeType so the
	// worker acks without writing partial garbage to content_text.
	if _, err := index.ExtractText("image/png", []byte{0x89, 0x50}); err == nil {
		t.Fatal("expected unsupported mime error for image/png")
	}
}
