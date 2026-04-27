package ws_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/api/ws"
)

// TestWSReceivesFileUploadEvent stands up a hub, dials a real
// WebSocket client through the auth-middleware-fronted ServeWS
// handler, broadcasts a file_upload event, and asserts the client
// receives the matching JSON envelope.
func TestWSReceivesFileUploadEvent(t *testing.T) {
	t.Parallel()

	const jwtSecret = "ws-test-secret"
	workspaceID := uuid.New()
	userID := uuid.New()

	hub := ws.NewHub()
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	go hub.Run(hubCtx)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.AuthMiddleware(jwtSecret))
		r.Get("/api/ws", ws.NewHandler(hub).ServeWS)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	token, _, err := middleware.IssueToken(jwtSecret, userID, workspaceID, "admin", time.Minute)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	// Convert http://... to ws://... — httptest.Server URLs are http.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	dialer := &gws.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial %s: %v (status=%d)", wsURL, err, resp.StatusCode)
		}
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	defer conn.Close()

	// The hub registers the client on a buffered channel; poll its
	// per-(workspaceID, userID) client count until the registration
	// has actually been processed before we broadcast. Avoids a
	// race where Broadcast finds an empty target set and silently
	// drops the event.
	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount(workspaceID, userID) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("client never registered with hub")
		}
		time.Sleep(10 * time.Millisecond)
	}

	want := ws.Event{
		Type: "file_upload",
		Payload: map[string]any{
			"id":    "file-1",
			"name":  "report.pdf",
			"by":    userID.String(),
			"bytes": 4096,
		},
	}
	if err := hub.Broadcast(workspaceID, userID, want); err != nil {
		t.Fatalf("broadcast: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}

	var got ws.Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal event: %v\nraw=%s", err, string(raw))
	}
	if got.Type != want.Type {
		t.Fatalf("type: got %q, want %q (raw=%s)", got.Type, want.Type, string(raw))
	}
	payload, ok := got.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload: got %T, want map[string]any (raw=%s)", got.Payload, string(raw))
	}
	if payload["id"] != "file-1" || payload["name"] != "report.pdf" {
		t.Fatalf("payload mismatch: %v (raw=%s)", payload, string(raw))
	}
}

// TestServeWSRejectsUnauthenticated asserts that ServeWS returns 401
// when invoked without a valid bearer token (i.e. the middleware
// chain never populates the auth context). Belt-and-braces over the
// auth middleware's own coverage; the WS handler shape makes this
// failure mode worth pinning explicitly.
func TestServeWSRejectsUnauthenticated(t *testing.T) {
	t.Parallel()

	hub := ws.NewHub()
	srv := httptest.NewServer(http.HandlerFunc(ws.NewHandler(hub).ServeWS))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}


