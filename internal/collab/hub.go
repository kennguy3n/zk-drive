package collab

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

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

// CollabRelayPublisher is the abstraction the hub uses to push a
// frame onto a multi-replica fan-out channel (typically Redis
// pub/sub). The concrete implementation lives in redis_relay.go;
// nil is treated as "single-replica mode" and the hub skips the
// publish call entirely.
//
// PublishFrame is called from the hub AFTER a successful local
// broadcast. The relay receives the same fully-framed payload
// and ships it to every replica. Each replica's relay subscribe
// loop then calls hub.BroadcastFromRelay to fan out to local
// clients in that document's room (if any).
//
// The interface keeps the hub agnostic of Redis — tests can
// supply a synchronous in-memory implementation and verify the
// publish-then-broadcast wiring without needing a Redis
// container.
type CollabRelayPublisher interface {
	PublishFrame(ctx context.Context, documentID uuid.UUID, payload []byte) error
}

// DocumentHub fans collab frames to per-document rooms. It is
// goroutine-safe. For single-replica deployments it operates
// entirely in-memory; for multi-replica deployments
// (CollabRelayPublisher wired) it also publishes every locally-
// broadcast frame to Redis pub/sub so every other replica's hub
// can fan the frame out to its local clients.
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

	// relay is the multi-replica fan-out publisher. nil for
	// single-replica deployments (no REDIS_URL configured). When
	// set, every successful local broadcast also publishes the
	// frame to Redis under `collab:{documentID}`, where the
	// other replicas' subscribe loops pick it up and fan out
	// locally via BroadcastFromRelay.
	relay CollabRelayPublisher

	// compactWG tracks in-flight compaction goroutines so
	// Shutdown can drain them before the server returns and the
	// underlying pool is closed. Without this WaitGroup, a
	// compaction goroutine could be mid-Compact (holding a pool
	// connection) when pool.Close() runs, producing a noisy
	// "acquire on closed pool" error and a lost compaction.
	compactWG sync.WaitGroup

	// shuttingDown is flipped to true by Shutdown under h.mu.Lock
	// before it calls compactWG.Wait. handleSyncUpdate must check
	// it under h.mu.RLock before calling compactWG.Add — otherwise
	// a readPump that races past AppendDelta could call Add(1)
	// after Wait has already observed counter==0, violating the
	// sync.WaitGroup contract and leaking a compaction goroutine
	// past pool.Close. The RWMutex acts as both the publication
	// barrier for the flag and the happens-before edge that
	// orders Add(1) before Wait.
	shuttingDown atomic.Bool
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

// WithRelay installs a multi-replica fan-out publisher. After
// every successful local broadcast (sync update or awareness),
// the hub also calls relay.PublishFrame so other replicas can
// fan the same frame out to their local clients. Returns the
// receiver for fluent chaining.
//
// A nil relay is permitted and disables multi-replica fan-out
// (single-replica mode). The hub never panics on a nil relay; it
// simply skips the PublishFrame call.
func (h *DocumentHub) WithRelay(relay CollabRelayPublisher) *DocumentHub {
	h.relay = relay
	return h
}

// Register adds c to its document's room. Idempotent: re-registering
// an already-registered client is a no-op (set semantics). Called
// by the HTTP upgrade path after authentication + permission check.
//
// Holds h.mu.Lock across the entire "lookup-or-create room AND
// insert client" sequence. Earlier revisions split this into two
// critical sections (h.mu around the map, r.mu around the insert)
// to keep h.mu short, but that introduced a phantom-room TOCTOU:
// a concurrent Unregister of the last client could observe an
// empty room between our h.mu.Unlock and our r.mu.Lock and delete
// the room from h.rooms, leaving our newly-inserted client in a
// room that no other goroutine can find. Since Register only runs
// once per WS upgrade (not per frame), holding h.mu for both steps
// has negligible cost.
func (h *DocumentHub) Register(c *DocumentClient) {
	c.hub = h
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[c.DocumentID]
	if !ok {
		r = newRoom()
		h.rooms[c.DocumentID] = r
	}
	r.mu.Lock()
	r.clients[c] = struct{}{}
	r.mu.Unlock()
}

// Unregister removes c from its document's room and signals the
// HTTP write pump to exit by closing c.done. Idempotent under
// races (closeOnce gates the channel close). Trailing empty rooms
// are garbage-collected so a workspace with millions of documents
// doesn't leak room maps after every editor disconnects.
//
// Holds h.mu.Lock across the "remove client, check empty, maybe
// delete room" sequence so the phantom-room race with Register
// (see Register's comment) cannot manifest. The cost is acceptable
// — Unregister fires once per WS disconnect.
func (h *DocumentHub) Unregister(c *DocumentClient) {
	h.mu.Lock()
	if r, ok := h.rooms[c.DocumentID]; ok && r != nil {
		r.mu.Lock()
		delete(r.clients, c)
		empty := len(r.clients) == 0
		r.mu.Unlock()
		if empty {
			delete(h.rooms, c.DocumentID)
		}
	}
	h.mu.Unlock()
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
// SyncStepUpdates frame. Used by tests and any path that needs to
// deliver a snapshot to an already-registered client.
//
// IMPORTANT: do NOT use this on the WS upgrade path — by the time
// SendSnapshot runs, the client is already visible to peer
// broadcastExcept calls, which can wedge a SyncUpdate ahead of the
// snapshot in the client's outbound FIFO. The upgrade path must
// use RegisterWithSnapshot, which atomically enqueues the snapshot
// AND inserts the client into the room under the same lock so the
// "snapshot first" invariant holds.
//
// The hub does NOT call documents.Snapshot itself: the HTTP layer
// fetches the bundle (so a snapshot read error surfaces during the
// upgrade handshake as a clean 5xx instead of a half-open WS
// connection). This function just frames + delivers the bytes.
func (h *DocumentHub) SendSnapshot(c *DocumentClient, yState []byte, tail [][]byte) {
	frame := encodeSnapshotFrame(yState, tail)
	h.deliverTo(c, frame)
}

// encodeSnapshotFrame builds a SyncStepUpdates frame containing a
// length-prefix bundle of (y_state, tail[0], tail[1], ...). Pure
// function; safe to call from any goroutine.
func encodeSnapshotFrame(yState []byte, tail [][]byte) []byte {
	bundle := AssembleSnapshotBundle(yState, tail)
	return EncodeSyncStepUpdates(bundle)
}

// RegisterWithSnapshot atomically inserts c into its document's
// room AND enqueues the cold-open snapshot frame into c.send
// under the same critical section. This is the only correct way
// to join a new client to an active room — using Register
// followed by SendSnapshot opens a race window where a concurrent
// broadcastExcept from a peer can wedge a SyncUpdate into c.send
// BEFORE the snapshot, violating the FIFO contract the client
// relies on (the snapshot establishes the Y.Doc baseline that
// every subsequent update is applied against).
//
// Lock ordering: h.mu.Lock → r.mu.Lock. The enqueue happens while
// r.mu.Lock is held, BEFORE r.clients[c] = {} makes the client a
// broadcast target. Any concurrent broadcastExcept must wait on
// r.mu.RLock, by which point the snapshot is already in c.send
// and the client's insertion has been published.
//
// The enqueue is non-blocking; c.send is freshly allocated with
// ClientSendBufferSize capacity (128), so a single frame can
// never block. If it somehow did (defensive only), we log and
// continue — the client will see an empty initial state and the
// next peer update / Y.Doc reconciliation will repair it.
func (h *DocumentHub) RegisterWithSnapshot(c *DocumentClient, yState []byte, tail [][]byte) {
	frame := encodeSnapshotFrame(yState, tail)
	c.hub = h
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[c.DocumentID]
	if !ok {
		r = newRoom()
		h.rooms[c.DocumentID] = r
	}
	r.mu.Lock()
	// Enqueue snapshot FIRST, then insert into the room. Both
	// happen under r.mu.Lock so no broadcastExcept can interleave.
	select {
	case c.send <- frame:
	default:
		// c.send has capacity ClientSendBufferSize and is freshly
		// created, so this should be unreachable in production.
		// Log defensively rather than blocking the lock holder.
		c.logger.Warn("collab: snapshot enqueue dropped on registration; buffer unexpectedly full")
	}
	r.clients[c] = struct{}{}
	r.mu.Unlock()
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
		return h.handleAwareness(ctx, c, f)
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

	// Multi-replica fan-out: after local broadcast, publish the
	// frame to Redis so any other replica with clients in this
	// document's room can fan it out locally too. The other
	// replicas have no concept of "originating client" — they
	// deliver to every member of their local room via
	// BroadcastFromRelay.
	//
	// We use the request ctx so a server shutdown that cancels
	// readPumps also cancels in-flight relay publishes; a Redis
	// outage that would otherwise block here is bounded by the
	// underlying redis.Client default timeouts (3 s read/write
	// by default).
	if h.relay != nil {
		if err := h.relay.PublishFrame(ctx, c.DocumentID, frame); err != nil {
			c.logger.Warn("collab: relay publish failed; multi-replica fan-out skipped for this frame", "err", err)
		}
	}

	// Compaction-due signal: only schedule on ServerSnapshotAllowed
	// folders. For strict_zk rooms, OpaqueConcatFold doesn't
	// reduce payload size — the y_state grows monotonically — so
	// triggering compaction every CompactionThreshold deltas
	// would waste cycles producing no net benefit. The
	// PendingDeltaCount keeps climbing and Compact runs only
	// when an admin / future "migrate folder" path triggers a
	// flush.
	if result.CompactionDue && c.Capability.ServerSnapshotAllowed && h.scheduleCompaction != nil {
		// Acquire h.mu.RLock as the publication barrier for
		// shuttingDown. If Shutdown has already flipped the
		// flag to true and called compactWG.Wait, we must NOT
		// call compactWG.Add(1) — that would violate the
		// sync.WaitGroup contract ("a positive delta when the
		// counter is zero must happen-before any Wait") and
		// leak a compaction goroutine past pool.Close.
		//
		// Holding the RLock for the Add(1) itself (not just the
		// flag read) is what gives us the happens-before edge:
		// Shutdown can only acquire h.mu.Lock once every RLock
		// holder has released, so its subsequent compactWG.Wait
		// is guaranteed to observe our Add.
		h.mu.RLock()
		if !h.shuttingDown.Load() {
			h.compactWG.Add(1)
			go func(ws, doc uuid.UUID) {
				defer h.compactWG.Done()
				h.scheduleCompaction(ws, doc)
			}(c.WorkspaceID, c.DocumentID)
		}
		h.mu.RUnlock()
	}
	return nil
}

func (h *DocumentHub) handleAwareness(ctx context.Context, c *DocumentClient, f Frame) error {
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
	if h.relay != nil {
		// Multi-replica fan-out for awareness frames. We thread
		// the caller's ctx (same one Handle / handleSyncUpdate
		// use) so a server shutdown that cancels readPumps also
		// cancels any in-flight awareness publish promptly,
		// instead of holding shutdown hostage for one Redis
		// WriteTimeout (typically 3s) per stuck publish. A
		// publish failure is warn-logged and dropped; awareness
		// is best-effort presence info, not durable state, so
		// a single dropped frame is recoverable on the client's
		// next awareness update.
		if err := h.relay.PublishFrame(ctx, c.DocumentID, frame); err != nil {
			c.logger.Warn("collab: relay publish failed for awareness frame", "err", err)
		}
	}
	return nil
}

// BroadcastFromRelay fans `payload` to every client in `documentID`'s
// room. It is called by the Redis collab relay's subscribe loop when
// ANOTHER replica publishes a SyncUpdate or Awareness frame for a
// document any of our local clients is editing.
//
// Crucially, this DOES NOT re-publish to Redis — the relay caller is
// the canonical "incoming from Redis" path, and re-publishing would
// create an infinite fan-out loop across replicas. The local
// handleSyncUpdate / handleAwareness paths handle the
// publish-to-Redis side of the wiring (via the relay's Publish
// method invoked from the hub).
//
// The relay's Subscribe loop drops the publishing replica's own
// echo via the per-relay originID prefix, so this method is only
// ever invoked for frames produced by a different replica. The
// local handleSyncUpdate path already fanned the frame to local
// peers (via broadcastExcept which skips the originating client);
// this method's job is to mirror that fan-out for clients
// connected to other replicas, NOT to round-trip frames back to
// the originating replica.
//
// All local clients in the room receive the frame. There is no
// "originating client" to skip because the originating client is
// connected to the publishing replica, not this one.
//
// Slow consumers (full outbound buffer) are unregistered to match
// the local broadcastExcept policy. The room lock is held for the
// minimum window required to snapshot the recipient set; the
// deliverTo calls happen outside the lock to avoid stalling
// concurrent Register / Unregister calls.
//
// payload is expected to be a fully-framed collab.Frame (i.e.
// EncodeSyncUpdate / EncodeAwareness output). The relay does NOT
// re-frame on Subscribe receive — the publisher framed it on its
// way out.
func (h *DocumentHub) BroadcastFromRelay(documentID uuid.UUID, payload []byte) {
	h.mu.RLock()
	r := h.rooms[documentID]
	h.mu.RUnlock()
	if r == nil {
		// No local clients in this room — nothing to do. This
		// is the common case for any document not being edited
		// on this replica.
		return
	}
	r.mu.RLock()
	targets := make([]*DocumentClient, 0, len(r.clients))
	for c := range r.clients {
		targets = append(targets, c)
	}
	r.mu.RUnlock()
	for _, c := range targets {
		h.deliverTo(c, payload)
	}
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
// The hub does not own the context the compaction scheduler
// captured, so Shutdown does not cancel that context — it relies
// on compactWG.Wait to drain in-flight jobs synchronously. The
// typical shutdown sequence in cmd/server is:
//
//  1. srv.Shutdown(ctx)       — stops accepting new HTTP/WS connections
//  2. collabHub.Shutdown()    — flips shuttingDown, closes existing WS clients,
//                              drains in-flight compactions
//  3. cancel()                — fires the global ctx.Done() (caller's responsibility,
//                              after Shutdown returns; not required for hub
//                              correctness)
//  4. bgGoroutines.Wait()     — drains other background goroutines
//  5. pool.Close()            — closes the DB pool against quiescent consumers
//
// Shutdown sets shuttingDown=true under h.mu.Lock BEFORE calling
// compactWG.Wait so any concurrent handleSyncUpdate that wants to
// schedule a new compaction sees the flag and skips the Add. The
// RWMutex serves both as the publication barrier for the flag and
// as the happens-before edge that orders any in-flight Add against
// Wait.
func (h *DocumentHub) Shutdown() {
	h.mu.Lock()
	h.shuttingDown.Store(true)
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
	// underlying pgx pool. Add(1) calls that win the race against
	// the shuttingDown flip are accounted for by compactWG; calls
	// that lose the race never executed Add.
	h.compactWG.Wait()
}
