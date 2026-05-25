package collab

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/folder"
)

// stubRepo is a hand-rolled document.Repository for hub tests. It
// only implements the methods AppendDelta + Compact's read paths
// touch — every other call panics so a regression that starts
// hitting an unstubbed path fails loudly.
type stubRepo struct {
	mu sync.Mutex

	// metadata is keyed by documentID and returns the minimal
	// fields the hub's AppendDelta path inspects.
	metadata map[uuid.UUID]*document.Document

	// appends counts AppendDelta calls per document so tests can
	// assert side effects without a real DB.
	appends map[uuid.UUID]int64

	// failNextAppend causes the next AppendDelta call to return
	// the configured error. Cleared after the failure fires.
	failNextAppend error

	// pendingCount is returned from CountDeltasAfter so tests can
	// drive the CompactionDue hint.
	pendingCount int64
}

func newStubRepo() *stubRepo {
	return &stubRepo{
		metadata: make(map[uuid.UUID]*document.Document),
		appends:  make(map[uuid.UUID]int64),
	}
}

func (s *stubRepo) Create(_ context.Context, _ *document.Document) error { panic("unused") }
func (s *stubRepo) GetByID(_ context.Context, _, _ uuid.UUID) (*document.Document, error) {
	panic("unused")
}
func (s *stubRepo) GetMetadata(_ context.Context, _, documentID uuid.UUID) (*document.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.metadata[documentID]
	if !ok {
		return nil, document.ErrNotFound
	}
	return d, nil
}
func (s *stubRepo) UpdateName(_ context.Context, _, _ uuid.UUID, _ string) (*document.Document, error) {
	panic("unused")
}
func (s *stubRepo) UpdateCollabMode(_ context.Context, _, _ uuid.UUID, _ string) (*document.Document, error) {
	panic("unused")
}
func (s *stubRepo) SoftDelete(_ context.Context, _, _ uuid.UUID) error { panic("unused") }
func (s *stubRepo) ListByFolder(_ context.Context, _, _ uuid.UUID) ([]*document.Document, error) {
	panic("unused")
}
func (s *stubRepo) ListByFolderSubtree(_ context.Context, _, _ uuid.UUID) ([]*document.Document, error) {
	panic("unused")
}
func (s *stubRepo) AppendDelta(_ context.Context, d *document.Delta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextAppend != nil {
		err := s.failNextAppend
		s.failNextAppend = nil
		return err
	}
	s.appends[d.DocumentID]++
	d.Seq = s.appends[d.DocumentID]
	return nil
}
func (s *stubRepo) ListDeltas(_ context.Context, _, _ uuid.UUID, _ int64, _ int) ([]*document.Delta, error) {
	panic("unused")
}
func (s *stubRepo) CountDeltasAfter(_ context.Context, _, _ uuid.UUID, _ int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingCount, nil
}
func (s *stubRepo) ReplaceSnapshot(_ context.Context, _, _ uuid.UUID, _, _ []byte, _, _ int64) (*document.Document, error) {
	panic("unused")
}
func (s *stubRepo) GetSnapshotBundle(_ context.Context, _, _ uuid.UUID, _ int) (*document.Document, []*document.Delta, error) {
	panic("unused")
}

// stubFolders implements document.FolderLookup. AppendDelta path
// doesn't call into folder lookup so we don't need a real folder
// store — but the type must satisfy the interface for newService.
type stubFolders struct {
	mu       sync.Mutex
	encMode  string
	folderID uuid.UUID
}

func (s *stubFolders) GetByID(_ context.Context, _, _ uuid.UUID) (*folder.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &folder.Folder{ID: s.folderID, EncryptionMode: s.encMode}, nil
}

func newServiceWithStubs(t *testing.T, docID uuid.UUID, encMode string) (*document.Service, *stubRepo) {
	t.Helper()
	repo := newStubRepo()
	repo.metadata[docID] = &document.Document{
		ID:         docID,
		FolderID:   uuid.New(),
		Name:       "test-doc",
		CollabMode: document.CollabModeRichPresence,
	}
	folders := &stubFolders{encMode: encMode}
	return document.NewService(repo, folders), repo
}

func TestHub_RegisterAndUnregister(t *testing.T) {
	docID := uuid.New()
	svc, _ := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	c := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	hub.Register(c)
	if got := hub.RoomSize(docID); got != 1 {
		t.Fatalf("expected room size 1, got %d", got)
	}
	hub.Unregister(c)
	if got := hub.RoomSize(docID); got != 0 {
		t.Fatalf("expected empty room after unregister, got %d", got)
	}
	// Subsequent Unregister calls must be no-ops (idempotent).
	hub.Unregister(c)
	select {
	case <-c.Done():
		// Expected — already closed by the first Unregister.
	default:
		t.Fatal("expected c.Done() to be closed")
	}
}

func TestHub_RoomIsolation(t *testing.T) {
	docA, docB := uuid.New(), uuid.New()
	svc, _ := newServiceWithStubs(t, docA, folder.EncryptionManagedEncrypted)
	// Insert the second doc into the stub metadata so AppendDelta
	// for docB doesn't 404.
	svc2 := svc
	hub := NewDocumentHub(svc2)
	wsID := uuid.New()

	cA1 := NewClient(wsID, uuid.New(), docA, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	cA2 := NewClient(wsID, uuid.New(), docA, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	cB1 := NewClient(wsID, uuid.New(), docB, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	hub.Register(cA1)
	hub.Register(cA2)
	hub.Register(cB1)

	if hub.RoomSize(docA) != 2 || hub.RoomSize(docB) != 1 {
		t.Fatalf("wrong room sizes: A=%d B=%d", hub.RoomSize(docA), hub.RoomSize(docB))
	}

	// Hand-craft an awareness broadcast from cA1; only cA2 should
	// see it. cB1 must not receive anything (different room).
	payload := []byte("cursor:42")
	frame := EncodeAwareness(payload)
	hub.broadcastExcept(cA1, frame)

	select {
	case got := <-cA2.Send():
		if !bytes.Equal(got, frame) {
			t.Fatalf("cA2 received wrong frame: %x", got)
		}
	case <-time.After(time.Second):
		t.Fatal("cA2 did not receive the awareness frame")
	}

	select {
	case got := <-cB1.Send():
		t.Fatalf("cB1 must not receive cross-room broadcast, got %x", got)
	case <-cA1.Send():
		t.Fatal("originator must not receive their own broadcast")
	case <-time.After(50 * time.Millisecond):
		// Expected — neither cB1 nor cA1 should receive anything.
	}
}

func TestHub_SyncUpdate_AppendsAndBroadcasts(t *testing.T) {
	docID := uuid.New()
	svc, repo := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	origin := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	peer := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	// Both clients need the same workspace_id for the AppendDelta
	// path; pin them.
	origin.WorkspaceID = uuid.New()
	peer.WorkspaceID = origin.WorkspaceID
	repo.metadata[docID].WorkspaceID = origin.WorkspaceID

	hub.Register(origin)
	hub.Register(peer)

	payload := []byte("yjs-update-payload")
	frame := Frame{Type: MessageSync, SubType: SyncUpdate, Payload: payload}
	if err := hub.Handle(context.Background(), origin, frame); err != nil {
		t.Fatalf("hub.Handle returned error: %v", err)
	}

	if got := repo.appends[docID]; got != 1 {
		t.Fatalf("expected 1 AppendDelta call, got %d", got)
	}

	select {
	case got := <-peer.Send():
		want := EncodeSyncUpdate(payload)
		if !bytes.Equal(got, want) {
			t.Fatalf("peer received wrong frame: %x", got)
		}
	case <-time.After(time.Second):
		t.Fatal("peer did not receive the broadcast")
	}

	// Originator must NOT echo back to itself.
	select {
	case got := <-origin.Send():
		t.Fatalf("originator received echo: %x", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_SyncUpdate_ReadOnlyDropped(t *testing.T) {
	docID := uuid.New()
	svc, repo := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	viewer := NewClient(uuid.New(), uuid.New(), docID, false /* canWrite */, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	hub.Register(viewer)

	err := hub.Handle(context.Background(), viewer, Frame{Type: MessageSync, SubType: SyncUpdate, Payload: []byte("blocked")})
	if !errors.Is(err, ErrUnauthorizedWrite) {
		t.Fatalf("expected ErrUnauthorizedWrite, got %v", err)
	}
	if got := repo.appends[docID]; got != 0 {
		t.Fatalf("read-only client triggered an AppendDelta (count=%d)", got)
	}
}

func TestHub_Awareness_DroppedForStrictZK(t *testing.T) {
	docID := uuid.New()
	svc, _ := newServiceWithStubs(t, docID, folder.EncryptionStrictZK)
	hub := NewDocumentHub(svc)

	// strict_zk: PresenceAllowed=false. The awareness frame must
	// be dropped server-side and never reach the peer.
	originator := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: false, ServerSnapshotAllowed: false})
	peer := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: false, ServerSnapshotAllowed: false})
	hub.Register(originator)
	hub.Register(peer)

	err := hub.Handle(context.Background(), originator, Frame{Type: MessageAwareness, Payload: []byte("cursor")})
	if !errors.Is(err, ErrPresenceNotAllowed) {
		t.Fatalf("expected ErrPresenceNotAllowed, got %v", err)
	}
	select {
	case got := <-peer.Send():
		t.Fatalf("strict_zk peer received awareness frame: %x", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_Awareness_BroadcastForManaged(t *testing.T) {
	docID := uuid.New()
	svc, _ := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	originator := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	peer := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	hub.Register(originator)
	hub.Register(peer)

	payload := []byte("{\"user\":\"alice\",\"cursor\":42}")
	if err := hub.Handle(context.Background(), originator, Frame{Type: MessageAwareness, Payload: payload}); err != nil {
		t.Fatalf("hub.Handle returned error: %v", err)
	}
	select {
	case got := <-peer.Send():
		want := EncodeAwareness(payload)
		if !bytes.Equal(got, want) {
			t.Fatalf("peer received wrong awareness frame")
		}
	case <-time.After(time.Second):
		t.Fatal("peer did not receive awareness frame")
	}
}

func TestHub_CompactionScheduled_OnManagedEncrypted(t *testing.T) {
	docID := uuid.New()
	svc, repo := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	repo.pendingCount = document.CompactionThreshold

	// Channel-based synchronization avoids the data race the race
	// detector flags when a bare uuid.UUID variable is written by
	// the scheduler goroutine and read by the test goroutine.
	schedCh := make(chan uuid.UUID, 1)
	hub := NewDocumentHub(svc).WithCompactionScheduler(func(_ uuid.UUID, d uuid.UUID) {
		schedCh <- d
	})

	origin := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	origin.WorkspaceID = uuid.New()
	repo.metadata[docID].WorkspaceID = origin.WorkspaceID
	hub.Register(origin)

	if err := hub.Handle(context.Background(), origin, Frame{Type: MessageSync, SubType: SyncUpdate, Payload: []byte("u")}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	select {
	case got := <-schedCh:
		if got != docID {
			t.Fatalf("scheduler invoked for wrong document: got %s want %s", got, docID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("compaction scheduler did not fire within 2s")
	}
}

func TestHub_CompactionNotScheduled_OnStrictZK(t *testing.T) {
	docID := uuid.New()
	svc, repo := newServiceWithStubs(t, docID, folder.EncryptionStrictZK)
	repo.pendingCount = document.CompactionThreshold

	var scheduled int32
	hub := NewDocumentHub(svc).WithCompactionScheduler(func(_ uuid.UUID, _ uuid.UUID) {
		atomic.AddInt32(&scheduled, 1)
	})

	origin := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: false, ServerSnapshotAllowed: false})
	origin.WorkspaceID = uuid.New()
	repo.metadata[docID].WorkspaceID = origin.WorkspaceID
	hub.Register(origin)

	if err := hub.Handle(context.Background(), origin, Frame{Type: MessageSync, SubType: SyncUpdate, Payload: []byte("u")}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	// Give any spurious scheduler enough time to fire.
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&scheduled) != 0 {
		t.Fatalf("scheduler must NOT fire on strict_zk; got %d invocations", scheduled)
	}
}

func TestHub_SendSnapshot_DeliversBundle(t *testing.T) {
	docID := uuid.New()
	svc, _ := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	c := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	hub.Register(c)

	state := []byte("STATE")
	tail := [][]byte{[]byte("D1"), []byte("D2")}
	hub.SendSnapshot(c, state, tail)

	select {
	case got := <-c.Send():
		if got[0] != MessageSync || got[1] != SyncStepUpdates {
			t.Fatalf("snapshot frame should be sync-step-updates, got %d/%d", got[0], got[1])
		}
		// Verify the bundle contents — strip the 2-byte header
		// then walk length prefixes.
		body := got[2:]
		want := [][]byte{state, tail[0], tail[1]}
		got2 := walkLengthPrefixed(t, body)
		if len(got2) != len(want) {
			t.Fatalf("wrong segment count: got %d want %d", len(got2), len(want))
		}
		for i, seg := range got2 {
			if !bytes.Equal(seg, want[i]) {
				t.Fatalf("segment %d mismatch", i)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot was not delivered")
	}
}

// TestHub_RegisterWithSnapshot_SnapshotFirst pins the invariant
// that the snapshot frame is the first frame in c.send even when
// a peer broadcasts an update racing against the new client's
// registration. We register an originator first, then concurrently
// (a) start a goroutine that pushes an update from the originator,
// and (b) call RegisterWithSnapshot for the joining client. After
// both complete we drain c.send and assert the FIRST frame is the
// sync-step-updates snapshot, not a sync-update from the peer.
func TestHub_RegisterWithSnapshot_SnapshotFirst(t *testing.T) {
	docID := uuid.New()
	svc, repo := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	originator := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	originator.WorkspaceID = uuid.New()
	repo.metadata[docID].WorkspaceID = originator.WorkspaceID
	hub.Register(originator)
	// Drain the originator's queue so we don't confuse our peer
	// frame with anything else.
	drainSend(originator)

	joiner := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	joiner.WorkspaceID = originator.WorkspaceID

	// Run RegisterWithSnapshot and the peer broadcast concurrently
	// to stress the ordering invariant. Repeat many iterations
	// across goroutines wouldn't help here — the invariant must
	// hold deterministically per-call, not statistically.
	state := []byte("BASELINE")
	tail := [][]byte{[]byte("D1")}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hub.Handle(context.Background(), originator, Frame{Type: MessageSync, SubType: SyncUpdate, Payload: []byte("peer-update")})
	}()
	hub.RegisterWithSnapshot(joiner, state, tail)
	<-done

	// First frame in joiner.send MUST be the snapshot
	// (sync-step-updates), regardless of whether the peer
	// broadcast landed before or after registration.
	select {
	case first := <-joiner.Send():
		if first[0] != MessageSync || first[1] != SyncStepUpdates {
			t.Fatalf("first frame in joiner.send must be sync-step-updates, got %d/%d", first[0], first[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame in joiner.send within 2s")
	}
}

func drainSend(c *DocumentClient) {
	for {
		select {
		case <-c.Send():
		default:
			return
		}
	}
}

func TestHub_SlowConsumer_Dropped(t *testing.T) {
	docID := uuid.New()
	svc, _ := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	originator := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	slow := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	hub.Register(originator)
	hub.Register(slow)

	// Fill the slow consumer's buffer to capacity.
	for i := 0; i < ClientSendBufferSize; i++ {
		slow.send <- []byte("warm")
	}
	// One more broadcast should drop slow because deliverTo's
	// default branch unregisters it.
	hub.broadcastExcept(originator, []byte("OVERFLOW"))

	select {
	case <-slow.Done():
		// Expected — slow was unregistered.
	case <-time.After(time.Second):
		t.Fatal("expected slow consumer to be unregistered")
	}
	if hub.RoomSize(docID) != 1 {
		t.Fatalf("expected room size 1 after slow drop, got %d", hub.RoomSize(docID))
	}
}

func TestHub_Shutdown_ClosesAllClients(t *testing.T) {
	docID := uuid.New()
	svc, _ := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)
	clients := make([]*DocumentClient, 0, 4)
	for i := 0; i < 4; i++ {
		c := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true})
		hub.Register(c)
		clients = append(clients, c)
	}
	hub.Shutdown()
	for i, c := range clients {
		select {
		case <-c.Done():
		case <-time.After(time.Second):
			t.Fatalf("client %d not closed by Shutdown", i)
		}
	}
	if hub.RoomSize(docID) != 0 {
		t.Fatalf("expected rooms empty after shutdown")
	}
}

func TestHub_AppendDeltaFailureDoesNotBroadcast(t *testing.T) {
	docID := uuid.New()
	svc, repo := newServiceWithStubs(t, docID, folder.EncryptionManagedEncrypted)
	hub := NewDocumentHub(svc)

	origin := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	peer := NewClient(uuid.New(), uuid.New(), docID, true, Capability{PresenceAllowed: true, ServerSnapshotAllowed: true})
	origin.WorkspaceID = uuid.New()
	peer.WorkspaceID = origin.WorkspaceID
	repo.metadata[docID].WorkspaceID = origin.WorkspaceID

	hub.Register(origin)
	hub.Register(peer)

	repo.failNextAppend = errors.New("simulated db failure")
	err := hub.Handle(context.Background(), origin, Frame{Type: MessageSync, SubType: SyncUpdate, Payload: []byte("u")})
	if err == nil {
		t.Fatal("expected error from failed AppendDelta")
	}
	// Peer must not have received a broadcast because persistence
	// failed.
	select {
	case got := <-peer.Send():
		t.Fatalf("peer received broadcast despite failed persist: %x", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func walkLengthPrefixed(t *testing.T, buf []byte) [][]byte {
	t.Helper()
	out := make([][]byte, 0)
	for len(buf) > 0 {
		if len(buf) < 4 {
			t.Fatalf("truncated prefix")
		}
		n := binary.BigEndian.Uint32(buf[:4])
		buf = buf[4:]
		if uint32(len(buf)) < n {
			t.Fatalf("truncated segment")
		}
		out = append(out, buf[:n])
		buf = buf[n:]
	}
	return out
}
