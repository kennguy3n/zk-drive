package changefeed_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/changefeed"
)

// fakeBroadcaster records every BroadcastJSONWorkspace call so the
// LocalPublisher test can assert the publish path serialises to the
// expected JSON shape and lands on the right workspace.
type fakeBroadcaster struct {
	mu     sync.Mutex
	last   uuid.UUID
	bodies [][]byte
}

func (f *fakeBroadcaster) BroadcastJSONWorkspace(workspaceID uuid.UUID, payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = workspaceID
	cp := make([]byte, len(payload))
	copy(cp, payload)
	f.bodies = append(f.bodies, cp)
}

func TestLocalPublisher_PublishMarshalsAndForwards(t *testing.T) {
	t.Parallel()

	bc := &fakeBroadcaster{}
	pub := changefeed.NewLocalPublisher(bc)

	wsID := uuid.New()
	mut := changefeed.Mutation{
		Sequence:    7,
		WorkspaceID: wsID,
		Kind:        changefeed.KindFile,
		Op:          changefeed.OpCreate,
		ResourceID:  uuid.New(),
		Name:        "x.txt",
	}
	if err := pub.Publish(context.Background(), wsID, changefeed.Event{Type: "change", Payload: mut}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if bc.last != wsID {
		t.Fatalf("broadcast workspace = %s, want %s", bc.last, wsID)
	}
	if len(bc.bodies) != 1 {
		t.Fatalf("got %d bodies, want 1", len(bc.bodies))
	}
	var got changefeed.Event
	if err := json.Unmarshal(bc.bodies[0], &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.Type != "change" {
		t.Fatalf("envelope type = %q, want change", got.Type)
	}
	if got.Payload.Sequence != 7 {
		t.Fatalf("payload sequence = %d, want 7", got.Payload.Sequence)
	}
	if got.Payload.Name != "x.txt" {
		t.Fatalf("payload name = %q, want x.txt", got.Payload.Name)
	}
}

func TestLocalPublisher_NilBroadcasterIsNoop(t *testing.T) {
	t.Parallel()

	pub := changefeed.NewLocalPublisher(nil)
	// Must not panic, must not error.
	if err := pub.Publish(context.Background(), uuid.New(), changefeed.Event{Type: "change"}); err != nil {
		t.Fatalf("publish on nil broadcaster: %v", err)
	}
}

func TestLocalPublisher_NilReceiverIsNoop(t *testing.T) {
	t.Parallel()

	var pub *changefeed.LocalPublisher
	if err := pub.Publish(context.Background(), uuid.New(), changefeed.Event{Type: "change"}); err != nil {
		t.Fatalf("publish on nil receiver: %v", err)
	}
}
