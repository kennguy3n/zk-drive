package collab

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/document"
)

// AgentUserID is a sentinel UUID used by the agent client to identify
// itself in delta authorship and awareness frames. All agent edits
// are attributed to this ID so collaborators can distinguish AI-
// generated changes from human edits in the document's delta history.
var AgentUserID = uuid.MustParse("00000000-0000-0000-0000-00000000a111")

// AgentClient is an in-process collab client that lets backend AI
// services manipulate a document through the same CRDT path as human
// editors. It registers with the DocumentHub as a regular
// DocumentClient (with CanWrite=true), so agent edits are:
//
//   - Persisted as deltas via documents.AppendDelta (same as WS clients).
//   - Broadcast in real-time to all connected editors via the hub.
//   - Undo-able via the CRDT undo stack on the client side.
//   - Subject to the same capability matrix (strict_zk → no agent).
//
// The agent client does NOT open a WebSocket — it calls the hub's
// Handle method directly with a decoded Frame, mimicking what
// collabReadPump does for a real WS connection. Outbound frames
// (broadcasts from other clients) are drained via a background
// goroutine that reads from client.Send() and discards them — the
// agent doesn't need to observe human edits because it builds its
// own Yjs state from the snapshot when it needs document content.
type AgentClient struct {
	client *DocumentClient
	hub    *DocumentHub
	done   chan struct{}
	once   sync.Once
}

// NewAgentClient creates and registers an agent client for the given
// document. The caller must ensure the document is not strict_zk and
// the agent has editor permission — this is enforced at the API
// handler level before calling this constructor.
//
// The snapshot bundle (yState + tailDeltas) is fetched and delivered
// to the client via RegisterWithSnapshot so the agent starts with the
// full document state, just like a human editor's WS connection.
//
// ctx bounds the snapshot fetch. The returned AgentClient must be
// Closed when the agent operation is complete to unregister from the
// hub and stop the drain goroutine.
func NewAgentClient(
	ctx context.Context,
	hub *DocumentHub,
	docs *document.Service,
	workspaceID, documentID uuid.UUID,
) (*AgentClient, error) {
	if hub == nil {
		return nil, errors.New("collab: agent client requires a non-nil hub")
	}
	if docs == nil {
		return nil, errors.New("collab: agent client requires a non-nil document service")
	}

	snap, err := docs.Snapshot(ctx, workspaceID, documentID)
	if err != nil {
		return nil, fmt.Errorf("collab: agent snapshot fetch: %w", err)
	}

	cap := FromDocumentCapability(snap.Capability)
	client := NewClient(workspaceID, AgentUserID, documentID, true, cap)

	// Build the tail-delta payload slice for the snapshot bundle.
	tail := make([][]byte, 0, len(snap.TailDeltas))
	for _, d := range snap.TailDeltas {
		tail = append(tail, d.Payload)
	}

	hub.RegisterWithSnapshot(client, snap.Document.YState, tail)

	ac := &AgentClient{
		client: client,
		hub:    hub,
		done:   make(chan struct{}),
	}

	// Drain outbound frames from other clients. The agent doesn't
	// need to process them, but the send buffer must be drained to
	// prevent the hub from unregistering the agent as a slow consumer.
	go ac.drain()

	return ac, nil
}

// drain consumes outbound frames from the hub broadcast and discards
// them. The agent builds its own document state from the snapshot and
// doesn't need to observe incremental updates from human editors.
func (ac *AgentClient) drain() {
	for {
		select {
		case _, ok := <-ac.client.Send():
			if !ok {
				return
			}
		case <-ac.client.Done():
			return
		case <-ac.done:
			return
		}
	}
}

// ApplyUpdate pushes a Yjs update payload through the hub's Handle
// method, which persists it as a delta and broadcasts to all other
// room members. This is the same path a WS client's SyncUpdate frame
// takes — the agent is just another editor.
//
// ctx controls the persistence deadline. The payload must be a valid
// encoded Yjs update (Y.encodeUpdate output).
func (ac *AgentClient) ApplyUpdate(ctx context.Context, payload []byte) error {
	if ac.client == nil {
		return errors.New("collab: agent client closed")
	}
	frame := Frame{
		Type:    MessageSync,
		SubType: SyncUpdate,
		Payload: payload,
	}
	return ac.hub.Handle(ctx, ac.client, frame)
}

// Client returns the underlying DocumentClient. Used by callers that
// need direct access to the client identity (e.g. for awareness).
func (ac *AgentClient) Client() *DocumentClient {
	return ac.client
}

// Close unregisters the agent from the hub and stops the drain
// goroutine. Idempotent via sync.Once.
func (ac *AgentClient) Close() {
	ac.once.Do(func() {
		close(ac.done)
		ac.hub.Unregister(ac.client)
	})
}
