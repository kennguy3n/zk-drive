package collab

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/document"
)

// TestYjsMergeFold_EmptyTailReturnsError mirrors the same
// precondition OpaqueConcatFold enforces — an empty tail would
// trigger a non-progressing ReplaceSnapshot, which the document
// service refuses with a constraint violation. Surfacing this as
// a fold-time error keeps the failure mode local and easy to
// trace.
func TestYjsMergeFold_EmptyTailReturnsError(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)
	fold := YjsMergeFold(rt)
	if _, _, _, err := fold(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("expected error for empty tail")
	}
}

// TestYjsMergeFold_NilRuntimeReturnsError pins the documented
// behaviour: a YjsMergeFold(nil) fold function panics-or-errors
// at call time rather than silently falling back to opaque
// concat (which would produce a different wire format than
// clients expect for managed_encrypted folders).
func TestYjsMergeFold_NilRuntimeReturnsError(t *testing.T) {
	t.Parallel()
	fold := YjsMergeFold(nil)
	tail := []*document.Delta{{Seq: 1, Payload: []byte{0x01}}}
	_, _, _, err := fold(context.Background(), nil, nil, tail)
	if err == nil {
		t.Fatal("expected error for nil runtime")
	}
}

// TestYjsMergeFold_ProducesValidSnapshot exercises the headline
// happy path: a current state + tail of two independent client
// updates folds into a single compact update whose state vector
// covers all three clients (state, A, B) and whose ApplyExtractText
// reproduces the union of inserted content.
func TestYjsMergeFold_ProducesValidSnapshot(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	currentState := makeUpdate(t, rt, 7, "STATE-")
	deltaA := makeUpdate(t, rt, 1, "A!")
	deltaB := makeUpdate(t, rt, 2, "B!")

	docID := uuid.New()
	wsID := uuid.New()
	tail := []*document.Delta{
		{DocumentID: docID, WorkspaceID: wsID, Seq: 10, Payload: deltaA},
		{DocumentID: docID, WorkspaceID: wsID, Seq: 11, Payload: deltaB},
	}

	fold := YjsMergeFold(rt)
	newState, newSV, upToSeq, err := fold(context.Background(), currentState, nil, tail)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(newState) == 0 {
		t.Fatal("expected non-empty merged state")
	}
	if len(newSV) == 0 {
		t.Fatal("expected non-empty state vector")
	}
	if upToSeq != 11 {
		t.Errorf("wrong upToSeq: got %d want 11", upToSeq)
	}

	// Apply the merged state to a fresh doc and verify all three
	// clients' content survives.
	got, err := rt.ApplyAndExtractText(context.Background(), newState)
	if err != nil {
		t.Fatalf("ApplyAndExtractText: %v", err)
	}
	if !bytes.Contains(got, []byte("STATE-")) {
		t.Errorf("merged state missing currentState content: got %q", got)
	}
	if !bytes.Contains(got, []byte("A!")) {
		t.Errorf("merged state missing deltaA content: got %q", got)
	}
	if !bytes.Contains(got, []byte("B!")) {
		t.Errorf("merged state missing deltaB content: got %q", got)
	}
}

// TestYjsMergeFold_NoCurrentStateAcceptsTailOnly exercises the
// first-compaction case: the document has no prior snapshot, so
// currentState is nil/empty and the fold must construct the
// initial snapshot purely from the tail.
func TestYjsMergeFold_NoCurrentStateAcceptsTailOnly(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	deltaA := makeUpdate(t, rt, 1, "first")

	tail := []*document.Delta{
		{Seq: 1, Payload: deltaA},
	}
	fold := YjsMergeFold(rt)
	newState, _, _, err := fold(context.Background(), nil, nil, tail)
	if err != nil {
		t.Fatalf("fold with empty currentState: %v", err)
	}

	got, err := rt.ApplyAndExtractText(context.Background(), newState)
	if err != nil {
		t.Fatalf("ApplyAndExtractText: %v", err)
	}
	if string(got) != "first" {
		t.Errorf("wrong merged content: got %q want %q", got, "first")
	}
}

// TestYjsMergeFold_ReducesPayloadSize verifies that the fold
// actually shrinks the bundle: the merged state for two updates
// of the same content should be smaller than the sum of the
// inputs because yrs eliminates redundant per-client headers and
// can normalise the insertion order.
//
// This is the property that motivated the migration from
// OpaqueConcatFold (which never shrinks — it just length-
// prefixes) to YjsMergeFold (which performs a real CRDT merge).
// If this assertion ever fires it means the wasm-side fold has
// silently regressed to a passthrough.
func TestYjsMergeFold_ReducesPayloadSize(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	// Same client editing the same text over many sequential
	// edits — yrs can collapse these into a single insertion
	// block, so the merged size is much smaller than the sum.
	const N = 32
	updates := make([][]byte, 0, N)
	totalIn := 0
	for i := 0; i < N; i++ {
		u := makeUpdate(t, rt, uint64(i+1), "x")
		updates = append(updates, u)
		totalIn += len(u)
	}

	merged, err := rt.MergeUpdates(context.Background(), updates)
	if err != nil {
		t.Fatalf("MergeUpdates: %v", err)
	}
	if len(merged) >= totalIn {
		t.Errorf("merge did not reduce size: merged=%d totalInputs=%d (compaction is ineffective)", len(merged), totalIn)
	}
}

// TestYjsMergeFold_FoldForRoutesToMerge pins the FoldFor routing
// for the managed-encrypted path with a non-nil runtime: it MUST
// return a fold that produces a compact merged update, NOT the
// length-prefix bundle OpaqueConcatFold produces. This is the
// behavioural difference clients see between the two folds.
func TestYjsMergeFold_FoldForRoutesToMerge(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)
	fold := FoldFor(Capability{ServerSnapshotAllowed: true}, rt)
	if fold == nil {
		t.Fatal("expected non-nil fold for ServerSnapshotAllowed=true with runtime")
	}

	deltaA := makeUpdate(t, rt, 1, "hello")
	tail := []*document.Delta{{Seq: 1, Payload: deltaA}}
	out, _, _, err := fold(context.Background(), nil, nil, tail)
	if err != nil {
		t.Fatalf("fold call: %v", err)
	}

	// Smoking gun: OpaqueConcatFold's output starts with a 4-byte
	// big-endian length prefix that, for a single-element tail,
	// equals len(payload). The yrs v1 update format starts with
	// a varint-encoded struct count which is never 0x00 0x00 0x00
	// for a non-empty doc. So if out[:4] equals the length of the
	// original delta in big-endian, the routing erroneously used
	// the opaque fold.
	if len(out) >= 4 {
		header := uint32(out[0])<<24 | uint32(out[1])<<16 | uint32(out[2])<<8 | uint32(out[3])
		if int(header) == len(deltaA) {
			t.Error("FoldFor with non-nil runtime appears to have produced an OpaqueConcatFold-style length-prefix bundle; expected a yrs v1 update")
		}
	}

	// The merged update must apply cleanly and reproduce the
	// original text content.
	text, err := rt.ApplyAndExtractText(context.Background(), out)
	if err != nil {
		t.Fatalf("ApplyAndExtractText on FoldFor result: %v", err)
	}
	if string(text) != "hello" {
		t.Errorf("wrong content: got %q want %q", text, "hello")
	}
}
