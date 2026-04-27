// Package ws implements the real-time notification fan-out used by
// the zk-drive web client. The Hub keeps a per-(workspace_id, user_id)
// set of WebSocket clients and pushes JSON envelopes onto each
// client's bounded send channel. Slow consumers are dropped (the
// connection is closed and unregistered) so a single misbehaving
// client cannot stall the broadcaster.
//
// ServeWS is mounted behind api/middleware.AuthMiddleware: the
// (workspaceID, userID) tuple is sourced from the JWT claims that the
// middleware injects into the request context. Origin checking is
// advisory because authentication has already happened upstream.
//
// Wire format (matches docs/ARCHITECTURE.md §X-realtime):
//
//	{"type": "notification", "payload": {"id": "...", "type": "share_link.created", "title": "...", "body": "..."}}
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/kennguy3n/zk-drive/api/middleware"
)

// Tunables for the read/write pumps. Values mirror the gorilla
// example chat server; pongWait is intentionally generous so mobile
// clients on flaky networks don't churn the hub by reconnecting on
// every transient stall.
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096
	sendBufferSize = 32
)

// Event is the JSON envelope pushed to clients. Type identifies the
// event family ("notification", "file_upload", ...); Payload is an
// arbitrary JSON-encodable value defined by the caller.
type Event struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// Client wraps a single WebSocket connection with a buffered send
// channel. Exposed so callers (notably tests) can build a Client out
// of an arbitrary *websocket.Conn without going through ServeWS.
//
// done is closed exactly once when the client is removed from the
// hub; the writePump and any concurrent BroadcastJSON select on it
// to bail out instead of writing into a closed send channel. This
// is the standard "closed-channel-as-broadcast" pattern: send is
// never closed (so we can never panic on send), and a closeOnce
// guards the close itself against double-close races between
// removeClient, closeAll, and the inline fallback in Unregister.
type Client struct {
	hub         *Hub
	conn        *websocket.Conn
	workspaceID uuid.UUID
	userID      uuid.UUID
	send        chan []byte
	done        chan struct{}
	closeOnce   sync.Once
}

// shutdown closes c.done at most once. Safe to call from any
// goroutine; subsequent calls are no-ops.
func (c *Client) shutdown() {
	c.closeOnce.Do(func() { close(c.done) })
}

// clientKey scopes the hub's client map to (workspace, user). Two
// users in the same workspace, or the same user logged in twice in
// different workspaces, never collide.
type clientKey struct {
	workspaceID uuid.UUID
	userID      uuid.UUID
}

// Hub fans out events to every client for a (workspaceID, userID)
// pair. Construct with NewHub and start with Run.
type Hub struct {
	mu         sync.RWMutex
	clients    map[clientKey]map[*Client]struct{}
	register   chan *Client
	unregister chan *Client
}

// NewHub returns a Hub ready to accept registrations once Run is
// started.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[clientKey]map[*Client]struct{}),
		register:   make(chan *Client, 64),
		unregister: make(chan *Client, 64),
	}
}

// Run is the hub's event loop. It returns when ctx is canceled. Call
// once in a dedicated goroutine.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			h.closeAll()
			return
		case c := <-h.register:
			h.addClient(c)
		case c := <-h.unregister:
			h.removeClient(c)
		}
	}
}

func (h *Hub) addClient(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := clientKey{c.workspaceID, c.userID}
	set, ok := h.clients[key]
	if !ok {
		set = make(map[*Client]struct{})
		h.clients[key] = set
	}
	set[c] = struct{}{}
}

func (h *Hub) removeClient(c *Client) {
	h.mu.Lock()
	key := clientKey{c.workspaceID, c.userID}
	set, ok := h.clients[key]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.clients, key)
	}
	h.mu.Unlock()
	// shutdown is idempotent (sync.Once) so unrelated callers
	// racing through removeClient / closeAll never double-close
	// c.done. Crucially we never close c.send: BroadcastJSON would
	// panic on send-after-close. The writePump drains c.send when
	// it sees c.done.
	c.shutdown()
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	victims := make([]*Client, 0, len(h.clients))
	for key, set := range h.clients {
		for c := range set {
			victims = append(victims, c)
		}
		delete(h.clients, key)
	}
	h.mu.Unlock()
	for _, c := range victims {
		c.shutdown()
	}
}

// Register adds c to the hub. Safe to call before Run starts; the
// register channel is buffered.
func (h *Hub) Register(c *Client) {
	h.register <- c
}

// Unregister removes c from the hub and signals the writePump to
// exit (by closing c.done — see the Client doc for why we never
// close c.send directly).
func (h *Hub) Unregister(c *Client) {
	select {
	case h.unregister <- c:
	default:
		// Channel full; do the bookkeeping inline so we never block
		// the connection goroutine on hub shutdown.
		h.removeClient(c)
	}
}

// Broadcast marshals event and pushes the JSON to every client for
// (workspaceID, userID). Slow consumers are unregistered (their send
// channel is full); fast consumers receive the event immediately.
func (h *Hub) Broadcast(workspaceID, userID uuid.UUID, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	h.BroadcastJSON(workspaceID, userID, payload)
	return nil
}

// ClientCount returns the number of clients currently registered
// for (workspaceID, userID). Primarily useful in tests that need to
// synchronise on registration before broadcasting.
func (h *Hub) ClientCount(workspaceID, userID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[clientKey{workspaceID, userID}])
}

// BroadcastJSON pushes an already-encoded JSON payload to every
// client for (workspaceID, userID). Useful when the encoded bytes
// were produced upstream (e.g. relayed from Redis pub/sub).
//
// The send select includes c.done as a sibling case so a client
// torn down concurrently with this loop is silently skipped instead
// of blocking forever (or, before the c.done refactor, panicking on
// a closed send channel). Slow consumers — those whose send buffer
// is full — are unregistered so the next broadcast does not retry.
func (h *Hub) BroadcastJSON(workspaceID, userID uuid.UUID, payload []byte) {
	h.mu.RLock()
	set := h.clients[clientKey{workspaceID, userID}]
	targets := make([]*Client, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		select {
		case c.send <- payload:
		case <-c.done:
			// Client was unregistered between the snapshot and the
			// send. Skip; whoever closed c.done already handled
			// removal from the map.
		default:
			// Slow consumer; drop and unregister. The unregister
			// chan is buffered, but we use a non-blocking send to
			// stay safe under hub shutdown.
			h.Unregister(c)
		}
	}
}

// NewClient constructs a Client around an already-upgraded
// *websocket.Conn. Tests use this to attach a connection to the hub
// without going through ServeWS.
func NewClient(hub *Hub, conn *websocket.Conn, workspaceID, userID uuid.UUID) *Client {
	return &Client{
		hub:         hub,
		conn:        conn,
		workspaceID: workspaceID,
		userID:      userID,
		send:        make(chan []byte, sendBufferSize),
		done:        make(chan struct{}),
	}
}

// Start registers c with the hub and launches the read and write
// pumps. The pumps run until the connection is closed or the hub
// shuts down. Callers should not touch the underlying conn after
// calling Start.
func (c *Client) Start() {
	c.hub.Register(c)
	go c.writePump()
	go c.readPump()
}

func (c *Client) readPump() {
	defer func() {
		c.hub.Unregister(c)
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		// Discard any client-sent frames; the channel is push-only
		// from the server's perspective. NextReader is the cheapest
		// way to drive read deadlines and the pong handler.
		if _, _, err := c.conn.NextReader(); err != nil {
			if !errors.Is(err, websocket.ErrCloseSent) &&
				!websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ws read: %v", err)
			}
			return
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case <-c.done:
			// Hub asked us to disconnect. Send a close frame and
			// exit; any unsent payloads still sitting in c.send
			// are dropped by design (the database row is the
			// source of truth — clients re-fetch on reconnect).
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// DefaultUpgrader is the upgrader used by ServeWS. CheckOrigin is
// permissive because auth is already enforced by the middleware and
// the JWT itself binds the connection to a workspace + user.
var DefaultUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// Handler exposes ServeWS as an http.Handler. It is constructed with
// a hub and (optionally) a custom upgrader.
type Handler struct {
	hub      *Hub
	upgrader websocket.Upgrader
}

// NewHandler returns a Handler bound to hub.
func NewHandler(hub *Hub) *Handler {
	return &Handler{hub: hub, upgrader: DefaultUpgrader}
}

// ServeWS upgrades the HTTP request to a WebSocket and registers the
// resulting client with the hub. The middleware chain that fronts
// this handler must populate (workspaceID, userID) on the request
// context — unauthenticated requests are rejected with 401.
func (h *Handler) ServeWS(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response on failure.
		return
	}
	c := NewClient(h.hub, conn, workspaceID, userID)
	c.Start()
}
