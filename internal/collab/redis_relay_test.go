package collab

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newTestRedis spins up an in-process miniredis instance and
// returns a redis.Client wired to it plus a cleanup function. We
// use miniredis (already a project dep, used by session +
// ratelimit tests) so CI doesn't need a real Redis container.
//
// miniredis v2.37 supports pub/sub (psubscribe + publish) which
// is what the relay needs.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// stubConsumer is a CollabRelayConsumer that records every
// BroadcastFromRelay invocation. Used in the subscribe-path tests
// because we don't need the full hub machinery — only that the
// relay decodes channel names and forwards the payload.
type stubConsumer struct {
	mu     sync.Mutex
	frames []relayDelivery
	count  atomic.Int64
}

type relayDelivery struct {
	documentID uuid.UUID
	payload    []byte
}

func (s *stubConsumer) BroadcastFromRelay(documentID uuid.UUID, payload []byte) {
	s.mu.Lock()
	s.frames = append(s.frames, relayDelivery{documentID: documentID, payload: append([]byte(nil), payload...)})
	s.mu.Unlock()
	s.count.Add(1)
}

func (s *stubConsumer) snapshot() []relayDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]relayDelivery, len(s.frames))
	copy(out, s.frames)
	return out
}

// TestRedisRelay_PublishAndSubscribeRoundtrip pins the headline
// contract: PublishFrame on one relay flows through Redis pub/sub
// to a second relay's Subscribe loop, which then calls
// consumer.BroadcastFromRelay with the original documentID +
// payload bytes.
func TestRedisRelay_PublishAndSubscribeRoundtrip(t *testing.T) {
	t.Parallel()
	rdb := newTestRedis(t)

	publisher := NewRedisCollabRelay(rdb)
	subscriber := NewRedisCollabRelay(rdb)

	consumer := &stubConsumer{}
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subErr := make(chan error, 1)
	go func() { subErr <- subscriber.Subscribe(subCtx, consumer) }()

	// Wait for the PSubscribe to be active — miniredis processes
	// PSUBSCRIBE synchronously but the goroutine needs a moment
	// to call Channel(). Polling with a short timeout avoids a
	// hardcoded sleep.
	docID := uuid.New()
	payload := []byte{0x00, 0x42, 0xAA, 0xBB, 0xCC}

	waitUntil(t, time.Second, func() bool {
		if err := publisher.PublishFrame(context.Background(), docID, payload); err != nil {
			t.Fatalf("publish: %v", err)
		}
		return consumer.count.Load() > 0
	})

	got := consumer.snapshot()
	if len(got) == 0 {
		t.Fatal("subscriber received no frames")
	}
	// Subscriber may have received >1 frame due to the polling
	// publish — only assert the first one matches.
	if got[0].documentID != docID {
		t.Errorf("documentID mismatch: got %s want %s", got[0].documentID, docID)
	}
	if string(got[0].payload) != string(payload) {
		t.Errorf("payload mismatch: got %x want %x", got[0].payload, payload)
	}

	cancel()
	select {
	case <-subErr:
		// expected — ctx canceled
	case <-time.After(2 * time.Second):
		t.Fatal("subscribe goroutine did not exit after cancel")
	}
}

// TestRedisRelay_MultipleReplicasFanOut models the
// production wiring: replica A publishes, replica B receives.
// Two distinct relay instances share the same Redis client so
// both Subscribe loops compete for the same pub/sub stream —
// pub/sub fan-out means BOTH replicas receive a copy.
func TestRedisRelay_MultipleReplicasFanOut(t *testing.T) {
	t.Parallel()
	rdb := newTestRedis(t)

	publisher := NewRedisCollabRelay(rdb)
	replicaA := NewRedisCollabRelay(rdb)
	replicaB := NewRedisCollabRelay(rdb)

	conA := &stubConsumer{}
	conB := &stubConsumer{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = replicaA.Subscribe(ctx, conA) }()
	go func() { _ = replicaB.Subscribe(ctx, conB) }()

	// Allow both subscribe loops to attach. We can't directly
	// observe psubscribe readiness from outside go-redis so we
	// publish in a tight loop until both consumers report a
	// receive. The publish is idempotent on the consumer side
	// (they each just append).
	docID := uuid.New()
	payload := []byte{0x00, 0xDE, 0xAD, 0xBE, 0xEF}
	waitUntil(t, time.Second, func() bool {
		if err := publisher.PublishFrame(context.Background(), docID, payload); err != nil {
			t.Fatalf("publish: %v", err)
		}
		return conA.count.Load() > 0 && conB.count.Load() > 0
	})

	for name, con := range map[string]*stubConsumer{"A": conA, "B": conB} {
		got := con.snapshot()
		if len(got) == 0 {
			t.Errorf("replica %s received no frames", name)
			continue
		}
		if got[0].documentID != docID {
			t.Errorf("replica %s documentID mismatch: got %s want %s", name, got[0].documentID, docID)
		}
		if string(got[0].payload) != string(payload) {
			t.Errorf("replica %s payload mismatch: got %x want %x", name, got[0].payload, payload)
		}
	}
}

// TestRedisRelay_PublishWithNilReceiverIsNoop pins the contract
// that single-replica fallback is safe: a hub configured with a
// nil relay must accept frame publishes without erroring or
// panicking.
func TestRedisRelay_PublishWithNilReceiverIsNoop(t *testing.T) {
	t.Parallel()
	var relay *RedisCollabRelay // nil
	if err := relay.PublishFrame(context.Background(), uuid.New(), []byte{0x01}); err != nil {
		t.Errorf("nil receiver PublishFrame: want nil err, got %v", err)
	}
}

// TestRedisRelay_NewRedisCollabRelayNilClient returns a nil
// relay so the hub can wire it unconditionally and treat it as
// single-replica mode.
func TestRedisRelay_NewRedisCollabRelayNilClient(t *testing.T) {
	t.Parallel()
	if r := NewRedisCollabRelay(nil); r != nil {
		t.Errorf("NewRedisCollabRelay(nil): want nil relay, got %v", r)
	}
}

// TestRedisRelay_SubscribeNilConsumerErrors pins the wiring-bug
// surfacing contract: a Subscribe call with no consumer must
// fail immediately rather than silently dropping every frame.
func TestRedisRelay_SubscribeNilConsumerErrors(t *testing.T) {
	t.Parallel()
	rdb := newTestRedis(t)
	relay := NewRedisCollabRelay(rdb)
	if err := relay.Subscribe(context.Background(), nil); err == nil {
		t.Fatal("Subscribe(nil consumer): want error, got nil")
	}
}

// TestRedisRelay_BadChannelDoesNotKillSubscribe pins the
// resilience guarantee: a producer accidentally PUBLISHing to a
// `collab:not-a-uuid` channel must NOT crash the relay's
// subscribe loop. Valid frames keep flowing after the bad one.
func TestRedisRelay_BadChannelDoesNotKillSubscribe(t *testing.T) {
	t.Parallel()
	rdb := newTestRedis(t)
	publisher := NewRedisCollabRelay(rdb)
	subscriber := NewRedisCollabRelay(rdb)

	consumer := &stubConsumer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = subscriber.Subscribe(ctx, consumer) }()

	// Try multiple bad publishes interleaved with a good one.
	// Use Publish directly (bypassing PublishFrame which only
	// emits valid UUIDs) to simulate the "rogue producer" case.
	docID := uuid.New()
	goodPayload := []byte{0x00, 0xAB, 0xCD}

	waitUntil(t, 2*time.Second, func() bool {
		_ = rdb.Publish(context.Background(), "collab:not-a-uuid", []byte{0xFF}).Err()
		_ = rdb.Publish(context.Background(), "collab:also-bad-12345", []byte{0xFE}).Err()
		_ = publisher.PublishFrame(context.Background(), docID, goodPayload)
		return consumer.count.Load() > 0
	})

	got := consumer.snapshot()
	if len(got) == 0 {
		t.Fatal("subscriber received no valid frames after bad publishes")
	}
	// Verify the valid frame made it through and bad ones did
	// not. We can detect bad frames in the snapshot because
	// they would have uuid.Nil documentID — but the relay
	// drops them BEFORE calling consumer, so they should never
	// appear in the snapshot at all.
	for _, d := range got {
		if d.documentID == uuid.Nil {
			t.Errorf("bad frame leaked into consumer with uuid.Nil document_id")
		}
	}
}

// TestRedisRelay_HubBroadcastFromRelayDeliversToLocalClients
// covers the consumer-side wiring: when the hub receives a
// frame from the relay, it MUST fan out to every local client
// in the document's room (and only that room).
func TestRedisRelay_HubBroadcastFromRelayDeliversToLocalClients(t *testing.T) {
	t.Parallel()

	docA := uuid.New()
	docB := uuid.New()
	wsID := uuid.New()
	// Re-use the hub_test stub factory — it produces a fully-
	// wired *document.Service whose AppendDelta etc. we don't
	// exercise here. BroadcastFromRelay only depends on the hub
	// rooms map; the service is held but not called by this
	// test case.
	svc, _ := newServiceWithStubs(t, docA, "managed_encrypted")
	hub := NewDocumentHub(svc)

	clientA1 := NewClient(wsID, uuid.New(), docA, true, Capability{ServerSnapshotAllowed: true})
	clientA2 := NewClient(wsID, uuid.New(), docA, true, Capability{ServerSnapshotAllowed: true})
	clientB := NewClient(wsID, uuid.New(), docB, true, Capability{ServerSnapshotAllowed: true})
	hub.Register(clientA1)
	hub.Register(clientA2)
	hub.Register(clientB)
	defer hub.Unregister(clientA1)
	defer hub.Unregister(clientA2)
	defer hub.Unregister(clientB)

	frame := []byte{0x00, 0xAA, 0xBB, 0xCC}
	hub.BroadcastFromRelay(docA, frame)

	got1 := drainOne(t, clientA1)
	got2 := drainOne(t, clientA2)
	if string(got1) != string(frame) {
		t.Errorf("clientA1: got %x want %x", got1, frame)
	}
	if string(got2) != string(frame) {
		t.Errorf("clientA2: got %x want %x", got2, frame)
	}

	// clientB is in a different room — must not receive.
	select {
	case unwanted := <-clientB.Send():
		t.Errorf("clientB received unexpected frame: %x", unwanted)
	case <-time.After(50 * time.Millisecond):
		// expected — no delivery to non-room members
	}
}

// drainOne reads exactly one frame from the client's send
// channel within a short timeout. Tests use this to verify
// fan-out delivery without dangling on the channel.
func drainOne(t *testing.T, c *DocumentClient) []byte {
	t.Helper()
	select {
	case f := <-c.Send():
		return f
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for frame on client %s", c.UserID)
		return nil
	}
}

// waitUntil polls fn() until it returns true or the timeout
// elapses. Used by the pub/sub tests because go-redis's
// PSUBSCRIBE handshake is async and we have no direct readiness
// signal — we publish in a loop until the subscriber's first
// message lands.
func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitUntil: condition not met within %s", timeout)
}
