package ws_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/api/ws"
	"github.com/kennguy3n/zk-drive/internal/workspace"
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
		// nil checker: this unit test exercises the stateless-JWT
		// path. The Redis-backed revocation flow has its own
		// dedicated integration test (logout_revocation_test.go).
		r.Use(middleware.AuthMiddleware(jwtSecret, nil))
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
	defer func() { _ = conn.Close() }()

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

// TestBroadcastRaceWithDisconnect spams Broadcast on one goroutine
// while a second goroutine repeatedly registers and unregisters
// clients for the same (workspaceID, userID) pair. Run with
// `go test -race` it pins the regression that closing c.send while
// BroadcastJSON had already snapshotted the target set used to
// panic with "send on closed channel". With the c.done refactor in
// place the test should complete cleanly.
func TestBroadcastRaceWithDisconnect(t *testing.T) {
	t.Parallel()

	hub := ws.NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	workspaceID := uuid.New()
	userID := uuid.New()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Producer: hammer Broadcast; never blocks.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = hub.Broadcast(workspaceID, userID, ws.Event{
				Type:    "race",
				Payload: map[string]any{"n": 1},
			})
		}
	}()

	// Consumer churn: register/unregister synthetic clients with
	// no live connection. We feed them a bare *websocket.Conn=nil
	// and avoid Start() (which spawns read/write pumps that would
	// panic on the nil conn). Using ws.NewClient + Hub.Register is
	// enough to reproduce the original race.
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c := ws.NewClient(hub, nil, workspaceID, userID)
			hub.Register(c)
			// Yield so the hub's Run loop has a chance to drain
			// the register channel before we tear down again.
			time.Sleep(50 * time.Microsecond)
			hub.Unregister(c)
		}
	}()

	// Run the producer for a fixed budget then shut down. 250 ms
	// is enough to hit the race on every prior reproduction; we
	// extend slightly so loaded CI hosts still cover the window.
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestBroadcastJSONWorkspace_FansToEveryUserInWorkspace registers
// two clients in the same workspace under different user IDs and a
// third client in a different workspace, then asserts the
// workspace-wide broadcast lands on the first two and not the
// third. This is the path the change-feed publisher takes: one
// mutation event must reach every connected user of the workspace
// irrespective of clientKey.userID.
func TestBroadcastJSONWorkspace_FansToEveryUserInWorkspace(t *testing.T) {
	t.Parallel()

	hub := ws.NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	wsA := uuid.New()
	wsB := uuid.New()
	userA1 := uuid.New()
	userA2 := uuid.New()
	userB1 := uuid.New()

	// Synthetic clients with nil *websocket.Conn — never Start()ed,
	// so the read/write pumps don't NPE; we only need the send
	// channel that NewClient allocates.
	c1 := ws.NewClient(hub, nil, wsA, userA1)
	c2 := ws.NewClient(hub, nil, wsA, userA2)
	c3 := ws.NewClient(hub, nil, wsB, userB1)
	hub.Register(c1)
	hub.Register(c2)
	hub.Register(c3)

	deadline := time.Now().Add(2 * time.Second)
	for hub.WorkspaceClientCount(wsA) != 2 || hub.WorkspaceClientCount(wsB) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("clients never registered: wsA=%d wsB=%d",
				hub.WorkspaceClientCount(wsA), hub.WorkspaceClientCount(wsB))
		}
		time.Sleep(10 * time.Millisecond)
	}

	payload := []byte(`{"type":"change","payload":{"sequence":42}}`)
	hub.BroadcastJSONWorkspace(wsA, payload)

	// Each wsA client gets the payload via its send chan; wsB
	// gets nothing.
	for label, c := range map[string]*ws.Client{"userA1": c1, "userA2": c2} {
		select {
		case got := <-c.Send():
			if string(got) != string(payload) {
				t.Fatalf("%s payload = %q, want %q", label, got, payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s never received broadcast", label)
		}
	}
	select {
	case got := <-c3.Send():
		t.Fatalf("wsB client received unexpected payload: %s", got)
	case <-time.After(100 * time.Millisecond):
		// expected: nothing for the other workspace
	}
}

// blockedIPChecker is an IPAllowChecker that denies every request, so
// the WS composition test can assert the upgrade is refused before the
// handshake completes.
type blockedIPChecker struct{}

func (blockedIPChecker) CheckAccess(_ context.Context, _ uuid.UUID, _ net.IP) error {
	return workspace.ErrIPBlocked
}

// TestServeWSEnforcesIPAllowlist asserts that the IP-allowlist
// middleware, composed in front of ServeWS the way cmd/server wires
// it, rejects a connection from a blocked network with 403 +
// X-ZkDrive-IP-Blocked BEFORE the WebSocket upgrade — closing the
// conditional-access bypass that previously left WS traffic ungated.
func TestServeWSEnforcesIPAllowlist(t *testing.T) {
	t.Parallel()

	hub := ws.NewHub()
	serveWS := ws.NewHandler(hub).ServeWS
	// Inject a workspace id the way the auth middleware does, then run
	// the IP-allowlist middleware in front of the handler.
	withWorkspace := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := middleware.WithWorkspaceID(r.Context(), uuid.New())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	handler := withWorkspace(middleware.IPAllowlist(blockedIPChecker{}, 1)(http.HandlerFunc(serveWS)))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	if resp.Header.Get(middleware.IPBlockedHeader) != "true" {
		t.Fatalf("expected %s: true header, got %q", middleware.IPBlockedHeader, resp.Header.Get(middleware.IPBlockedHeader))
	}
}

// suspendedChecker is a WorkspaceSuspensionChecker that reports every
// workspace as suspended, so the WS composition test can assert the
// upgrade is refused with 503 before the handshake completes.
type suspendedChecker struct{}

func (suspendedChecker) WorkspaceSuspension(_ context.Context, _ uuid.UUID) (bool, string, error) {
	return true, "nonpayment", nil
}

// TestServeWSEnforcesSuspensionGuard asserts that SuspensionGuard,
// composed in front of ServeWS the way cmd/server wires it, rejects a
// connection for a suspended workspace with 503 BEFORE the WebSocket
// upgrade — so a suspended workspace cannot keep realtime sync / collab
// alive while every REST call already returns 503.
func TestServeWSEnforcesSuspensionGuard(t *testing.T) {
	t.Parallel()

	hub := ws.NewHub()
	serveWS := ws.NewHandler(hub).ServeWS
	withWorkspace := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := middleware.WithWorkspaceID(r.Context(), uuid.New())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	handler := withWorkspace(middleware.SuspensionGuard(suspendedChecker{})(http.HandlerFunc(serveWS)))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	var body struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "workspace_suspended" {
		t.Fatalf("error: got %q, want workspace_suspended", body.Error)
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}
