package collab

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/document"
)

// ClientSendBufferSize sets the bounded outbound queue per client.
// A slow consumer that fills the buffer is dropped (Unregister'd)
// so a single misbehaving editor cannot stall the room. Sized
// generously vs api/ws (32) because collab traffic is bursty:
// applying a multi-character paste or compaction trigger pushes
// several frames in flight before the writer drains.
const ClientSendBufferSize = 128

// Capability captures the subset of internal/document.Capability
// that affects routing decisions inside the hub. We don't import
// the document.Capability struct directly because hub_test.go
// constructs fake clients with synthetic capabilities — a separate
// alias type makes the test surface explicit.
//
// PresenceAllowed gates MessageAwareness fan-out. ServerSnapshotAllowed
// gates the Compact trigger (we don't schedule compaction on
// strict_zk rooms because the OpaqueConcatFold doesn't reduce
// payload size — it just grows y_state — so trimming the tail
// would lose data the client still needs).
type Capability struct {
	PresenceAllowed       bool
	ServerSnapshotAllowed bool
}

// FromDocumentCapability lifts a document.Capability into the hub's
// subset. Defined so api/drive callers can pass the result of
// document.ResolveCapability without a manual struct copy.
func FromDocumentCapability(c document.Capability) Capability {
	return Capability{
		PresenceAllowed:       c.PresenceAllowed,
		ServerSnapshotAllowed: c.ServerSnapshotAllowed,
	}
}

// DocumentClient wraps a single editor's outbound queue + identity.
// The HTTP layer (api/drive/collab.go) constructs one of these per
// upgraded WebSocket connection and feeds inbound frames into the
// hub via Hub.Handle. The hub drives outbound frames into
// client.send; the HTTP layer's writePump drains the channel onto
// the wire.
//
// The hub never closes client.send (matches api/ws/handler.go's
// pattern — closing would race with concurrent Broadcast goroutines
// trying to send into it). Instead we close client.done exactly
// once, and the HTTP writer pump selects on c.done as a sibling
// case to drain-and-exit safely.
type DocumentClient struct {
	// hub is the back-reference set by Register. Not exported.
	hub *DocumentHub

	// WorkspaceID + UserID + DocumentID identify the client. The
	// hub uses DocumentID as the room key and UserID for awareness
	// fan-out filtering (a client never receives its own awareness
	// echoes — see deliver below).
	WorkspaceID uuid.UUID
	UserID      uuid.UUID
	DocumentID  uuid.UUID

	// CanWrite is true if the user has at least RoleEditor on the
	// document's parent folder. The hub enforces this per-frame on
	// inbound SyncUpdate / MessageAwareness frames: a read-only
	// (RoleViewer) client connects to observe but its update
	// frames are silently dropped server-side. We don't disconnect
	// — read-only collab is a legitimate use case (think of
	// a "see-only" share link).
	CanWrite bool

	// Capability snapshots the document's folder capability at
	// connect time. Folder.encryption_mode is IMMUTABLE per the
	// P2 privacy boundary (only the "migrate folder" admin
	// endpoint changes it, and that path forcibly disconnects
	// all collab clients), so a snapshot is safe — capability
	// cannot drift underneath the connection.
	Capability Capability

	// send is the outbound queue. Drained by the HTTP write pump.
	send chan []byte

	// done is closed when the hub removes the client. The HTTP
	// write pump selects on it to exit promptly.
	done chan struct{}

	// closeOnce ensures done is closed at most once even when
	// concurrent removeClient / closeAll races happen.
	closeOnce sync.Once

	// logger carries (document_id, user_id, workspace_id) for
	// every log line emitted on this client's behalf.
	logger *slog.Logger
}

// NewClient constructs a DocumentClient. Tests use it directly to
// register synthetic clients; production wires through ServeCollab
// in api/drive/collab.go.
func NewClient(workspaceID, userID, documentID uuid.UUID, canWrite bool, cap Capability) *DocumentClient {
	return &DocumentClient{
		WorkspaceID: workspaceID,
		UserID:      userID,
		DocumentID:  documentID,
		CanWrite:    canWrite,
		Capability:  cap,
		send:        make(chan []byte, ClientSendBufferSize),
		done:        make(chan struct{}),
		logger: slog.Default().With(
			"subsystem", "collab",
			"workspace_id", workspaceID.String(),
			"user_id", userID.String(),
			"document_id", documentID.String(),
		),
	}
}

// Send returns the receive-only end of the outbound queue. The HTTP
// write pump reads from this channel and writes binary frames to
// the *websocket.Conn. Tests also drain Send to assert what frames
// were delivered.
func (c *DocumentClient) Send() <-chan []byte { return c.send }

// Done returns a channel that is closed when the hub removes this
// client. The HTTP write pump selects on Done to exit promptly
// when the hub or server is shutting down.
func (c *DocumentClient) Done() <-chan struct{} { return c.done }

func (c *DocumentClient) shutdown() {
	c.closeOnce.Do(func() { close(c.done) })
}

// roomKey is the per-document set of clients. Stored as a
// map[*DocumentClient]struct{} (set semantics) so the hub doesn't
// need to track an index for unregister — pointer identity is
// the key.
type room struct {
	mu      sync.RWMutex
	clients map[*DocumentClient]struct{}
}

func newRoom() *room {
	return &room{clients: make(map[*DocumentClient]struct{})}
}

// DocumentHub fans collab frames to per-document rooms. It is
// goroutine-safe and operates entirely in-memory — multi-replica
// fan-out is deferred to P2e (the same Redis-pub/sub blueprint
// api/ws's changefeed publisher uses will land here too).
//
// The hub doesn't own the document.Service directly; instead, the
// HTTP layer hands the hub an inbound frame plus the necessary
// service + client metadata via Handle. This keeps the hub
// agnostic of HTTP concerns and makes testing trivial — a unit
// test wires a synthetic *document.Service stub and a few in-
// memory DocumentClients.
type DocumentHub struct {
	mu    sync.RWMutex
	rooms map[uuid.UUID]*room

	// documents is the persistence + capability gate the hub
	// consults for inbound SyncUpdate frames. The hub calls
	// documents.AppendDelta on every incoming update; if that
	// returns ErrCollabModeNotAllowed (because the document was
	// just set to disabled), the hub forwards a structured close
	// reason to the originating client and unregisters it.
	documents *document.Service

	// scheduleCompaction is an optional callback. When the hub
	// observes an AppendDeltaResult with CompactionDue=true, it
	// invokes scheduleCompaction(workspaceID, documentID) in a
	// fire-and-forget goroutine. The callback is supplied by the
	// HTTP wiring (typically a goroutine launcher that calls
	// documents.Compact with the appropriate FoldFunc). Nil-safe:
	// when unset, compaction-due signals are dropped silently and
	// the next caller will retry.
	scheduleCompaction func(workspaceID, documentID uuid.UUID)

	// compactWG tracks in-flight compaction goroutines so
	// Shutdown can drain them before the server returns and the
	// underlying pool is closed. Without this WaitGroup, a
	// compaction goroutine could be mid-Compact (holding a pool
	// connection) when pool.Close() runs, producing a noisy
	// "acquire on closed pool" error and a lost compaction.
	compactWG sync.WaitGroup
}

// NewDocumentHub constructs a hub. The documents service must be
// non-nil — the hub has nothing to do without it (no persistence,
// no capability resolution).
func NewDocumentHub(docs *document.Service) *DocumentHub {
	if docs == nil {
		panic("collab: NewDocumentHub requires a non-nil document.Service")
	}
	return &DocumentHub{
		rooms:     make(map[uuid.UUID]*room),
		documents: docs,
	}
}

// WithCompactionScheduler installs a callback the hub invokes when a
// delta-append crosses the compaction threshold. Returns the
// receiver for fluent chaining.
func (h *DocumentHub) WithCompactionScheduler(fn func(workspaceID, documentID uuid.UUID)) *DocumentHub {
	h.scheduleCompaction = fn
	return h
}

// Register adds c to its document's room. Idempotent: re-registering
// an already-registered client is a no-op (set semantics). Called
// by the HTTP upgrade path after authentication + permission check.
func (h *DocumentHub) Register(c *DocumentClient) {
	c.hub = h
	h.mu.Lock()
	r, ok := h.rooms[c.DocumentID]
	if !ok {
		r = newRoom()
		h.rooms[c.DocumentID] = r
	}
	h.mu.Unlock()
	r.mu.Lock()
	r.clients[c] = struct{}{}
	r.mu.Unlock()
}

// Unregister removes c from its document's room and signals the
// HTTP write pump to exit by closing c.done. Idempotent under
// races (closeOnce gates the channel close). Trailing empty rooms
// are garbage-collected so a workspace with millions of documents
// doesn't leak room maps after every editor disconnects.
func (h *DocumentHub) Unregister(c *DocumentClient) {
	h.mu.RLock()
	r := h.rooms[c.DocumentID]
	h.mu.RUnlock()
	if r != nil {
		r.mu.Lock()
		delete(r.clients, c)
		empty := len(r.clients) == 0
		r.mu.Unlock()
		if empty {
			// Race-safe room cleanup: re-acquire the hub write
			// lock, re-check emptiness (a concurrent Register
			// for the same doc could have repopulated the room
			// between our check and the delete), and only then
			// delete the map entry.
			h.mu.Lock()
			if rr, ok := h.rooms[c.DocumentID]; ok && rr == r {
				rr.mu.RLock()
				stillEmpty := len(rr.clients) == 0
				rr.mu.RUnlock()
				if stillEmpty {
					delete(h.rooms, c.DocumentID)
				}
			}
			h.mu.Unlock()
		}
	}
	c.shutdown()
}

// RoomSize returns the number of clients currently in the room for
// `documentID`. Primarily for tests that synchronise on registration
// before broadcasting. Returns 0 for unknown documents.
func (h *DocumentHub) RoomSize(documentID uuid.UUID) int {
	h.mu.RLock()
	r := h.rooms[documentID]
	h.mu.RUnlock()
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// SendSnapshot pushes the cold-start bundle (snapshot + tail
// deltas, length-prefix framed) to a single client as a
// SyncStepUpdates frame. Called by the HTTP upgrade path after
// Register so the joining editor can rebuild its Y.Doc immediately
// without an extra HTTP round-trip.
//
// The hub does NOT call documents.Snapshot itself: the HTTP layer
// fetches the bundle (so a snapshot read error surfaces during the
// upgrade handshake as a clean 5xx instead of a half-open WS
// connection). This function just frames + delivers the bytes.
func (h *DocumentHub) SendSnapshot(c *DocumentClient, yState []byte, tail [][]byte) {
	bundle := AssembleSnapshotBundle(yState, tail)
	frame := EncodeSyncStepUpdates(bundle)
	h.deliverTo(c, frame)
}

// ErrCollabDisabled is returned by Handle when an inbound frame
// targets a document whose collab_mode is 'disabled'. The HTTP
// layer should close the connection with code 1008 (Policy
// Violation) so the client doesn't auto-reconnect into the same
// failure mode.
var ErrCollabDisabled = errors.New("collab: document is disabled")

// ErrUnauthorizedWrite is returned by Handle when a viewer-only
// client sends a SyncUpdate or MessageAwareness frame. The hub
// silently drops these frames in normal operation; the error is
// returned so the HTTP layer can decide whether to log an audit
// event (it does — see api/drive/collab.go).
var ErrUnauthorizedWrite = errors.New("collab: client lacks editor permission")

// ErrPresenceNotAllowed is returned when a strict_zk client sends
// an awareness frame. The hub drops it silently because the
// client may simply have stale capability state, but the error is
// exposed so tests can assert the gating.
var ErrPresenceNotAllowed = errors.New("collab: presence not allowed by folder encryption mode")

// Handle dispatches an inbound decoded Frame from a registered
// client. The hub:
//
//  1. Enforces the capability matrix (viewer-only clients can't
//     append; strict_zk clients can't send awareness frames).
//  2. Persists SyncUpdate payloads via documents.AppendDelta —
//     the call is synchronous on the WS goroutine so the client
//     receives backpressure if the database stalls.
//  3. Broadcasts the persisted update (or the raw awareness
//     frame) to every OTHER client in the same room — never
//     echoes back to the originator.
//  4. Triggers compaction-due signals via scheduleCompaction in a
//     fire-and-forget goroutine when the persisted append crosses
//     the threshold AND ServerSnapshotAllowed is true.
//
// Returns nil on success or a typed error the HTTP layer can
// use to drive close codes / audit logs. Sync-step-1 frames are
// silently ignored (P2b's "dumb relay" mode pre-pushes the full
// snapshot in SendSnapshot, so the client doesn't need the
// server to compute a diff).
func (h *DocumentHub) Handle(ctx context.Context, c *DocumentClient, f Frame) error {
	switch f.Type {
	case MessageSync:
		return h.handleSync(ctx, c, f)
	case MessageAwareness:
		return h.handleAwareness(c, f)
	case MessageAuth:
		// Reserved; ignore in P2b. A future re-auth flow will
		// dispatch on the sub-type here.
		return nil
	default:
		return errors.New("collab: unrecognised message type")
	}
}

func (h *DocumentHub) handleSync(ctx context.Context, c *DocumentClient, f Frame) error {
	switch f.SubType {
	case SyncStepStateVector:
		// In P2b's relay mode we already pushed the full bundle
		// via SendSnapshot at connect time. A SyncStepStateVector
		// frame from the client is a no-op — we have nothing
		// additional to send. A future server-side merge fold
		// would use the client's state vector to compute a
		// precise diff here; for now the client over-receives,
		// which is correct because Y.applyUpdate is idempotent.
		return nil
	case SyncStepUpdates:
		// Server-typed; clients should never originate these.
		// Drop silently — a buggy client mirroring its own
		// inbound frames would otherwise loop a broadcast back
		// to peers through the persistence layer.
		return nil
	case SyncUpdate:
		return h.handleSyncUpdate(ctx, c, f.Payload)
	default:
		return errors.New("collab: unknown sync sub-type")
	}
}

func (h *DocumentHub) handleSyncUpdate(ctx context.Context, c *DocumentClient, payload []byte) error {
	if !c.CanWrite {
		// Read-only collab — drop the write silently. We don't
		// disconnect because the client may be intentionally
		// observing (e.g. a viewer share link). The HTTP layer
		// audits the drop.
		return ErrUnauthorizedWrite
	}
	result, err := h.documents.AppendDelta(ctx, document.AppendDeltaInput{
		WorkspaceID:  c.WorkspaceID,
		DocumentID:   c.DocumentID,
		Payload:      payload,
		AuthorUserID: c.UserID,
	})
	if err != nil {
		// Map document-layer policy errors to a typed close
		// signal. The HTTP layer translates this into a
		// 1008 (Policy Violation) close code.
		if errors.Is(err, document.ErrCollabModeNotAllowed) {
			return ErrCollabDisabled
		}
		return err
	}
	// Broadcast the persisted update to every OTHER member of the
	// room. We use the originating payload bytes verbatim (NOT
	// the seq) because Yjs updates are content-addressed —
	// applying the same update twice is a no-op on the client.
	// The hub does NOT echo the frame back to the originator;
	// the client already applied it locally before sending.
	frame := EncodeSyncUpdate(payload)
	h.broadcastExcept(c, frame)

	// Compaction-due signal: only schedule on ServerSnapshotAllowed
	// folders. For strict_zk rooms, OpaqueConcatFold doesn't
	// reduce payload size — the y_state grows monotonically — so
	// triggering compaction every CompactionThreshold deltas
	// would waste cycles producing no net benefit. The
	// PendingDeltaCount keeps climbing and Compact runs only
	// when an admin / future "migrate folder" path triggers a
	// flush.
	if result.CompactionDue && c.Capability.ServerSnapshotAllowed && h.scheduleCompaction != nil {
		h.compactWG.Add(1)
		go func(ws, doc uuid.UUID) {
			defer h.compactWG.Done()
			h.scheduleCompaction(ws, doc)
		}(c.WorkspaceID, c.DocumentID)
	}
	return nil
}

func (h *DocumentHub) handleAwareness(c *DocumentClient, f Frame) error {
	if !c.Capability.PresenceAllowed {
		// strict_zk room — drop the awareness frame server-side.
		// We don't close the connection; a TipTap client that
		// optimistically sends awareness on every keystroke
		// would churn-disconnect, and the legitimate fallback
		// (sync-only collab) keeps working as long as we just
		// silently discard the awareness traffic.
		return ErrPresenceNotAllowed
	}
	if !c.CanWrite {
		// Viewer-only clients can observe presence (they receive
		// awareness fan-out) but can't BROADCAST their own. A
		// viewer with a cursor would leak who's reading; we
		// require RoleEditor to publish presence.
		return ErrUnauthorizedWrite
	}
	frame := EncodeAwareness(f.Payload)
	h.broadcastExcept(c, frame)
	return nil
}

// broadcastExcept fans `payload` to every client in `from`'s room
// except `from` itself. Slow consumers are unregistered (matches
// the api/ws hub's drop-the-slowest policy).
func (h *DocumentHub) broadcastExcept(from *DocumentClient, payload []byte) {
	h.mu.RLock()
	r := h.rooms[from.DocumentID]
	h.mu.RUnlock()
	if r == nil {
		return
	}
	r.mu.RLock()
	targets := make([]*DocumentClient, 0, len(r.clients))
	for c := range r.clients {
		if c == from {
			continue
		}
		targets = append(targets, c)
	}
	r.mu.RUnlock()
	for _, c := range targets {
		h.deliverTo(c, payload)
	}
}

// deliverTo performs a single bounded send to a client's outbound
// queue. Slow consumers (full buffer) are unregistered so the
// next broadcast doesn't pay the latency cost of trying them
// again. Mirrors api/ws/handler.go's deliver pattern, including
// the c.done sibling case so a concurrently-unregistered client
// doesn't deadlock the broadcaster.
func (h *DocumentHub) deliverTo(c *DocumentClient, payload []byte) {
	select {
	case c.send <- payload:
	case <-c.done:
		// Client was unregistered between the snapshot and the
		// send. Skip; whoever closed c.done already handled
		// removal from the room.
	default:
		// Slow consumer; drop and unregister.
		c.logger.Warn("collab: client send buffer full, dropping connection")
		h.Unregister(c)
	}
}

// Shutdown closes every active client, clears the rooms map, and
// blocks until in-flight compaction goroutines return. Called from
// cmd/server's graceful-shutdown path so collab connections drain
// (and any compaction job in progress finishes) before the process
// closes the database pool.
//
// The caller is responsible for first cancelling the context the
// compaction scheduler captured — Shutdown does not cancel that
// context itself because the hub does not own it. The typical
// shutdown sequence in cmd/server is:
//
//  1. srv.Shutdown(ctx)       — stops accepting new HTTP/WS connections
//  2. collabHub.Shutdown()    — closes existing WS clients, drains compactions
//  3. cancel()                — fires the global ctx.Done()
//  4. bgGoroutines.Wait()     — drains other background goroutines
//  5. pool.Close()            — closes the DB pool against quiescent consumers
func (h *DocumentHub) Shutdown() {
	h.mu.Lock()
	victims := make([]*DocumentClient, 0)
	for docID, r := range h.rooms {
		r.mu.Lock()
		for c := range r.clients {
			victims = append(victims, c)
		}
		r.clients = nil
		r.mu.Unlock()
		delete(h.rooms, docID)
	}
	h.mu.Unlock()
	for _, c := range victims {
		c.shutdown()
	}
	// Wait for any in-flight compaction goroutines to finish so
	// the caller can safely close the document.Service and the
	// underlying pgx pool.
	h.compactWG.Wait()
}
