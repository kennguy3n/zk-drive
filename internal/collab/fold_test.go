package collab

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/document"
)

func TestOpaqueConcatFold_EmptyTailReturnsError(t *testing.T) {
	if _, _, _, err := OpaqueConcatFold(nil, nil, nil); err == nil {
		t.Fatal("expected error for empty tail")
	}
}

func TestOpaqueConcatFold_PrefixesStateAndDeltas(t *testing.T) {
	state := []byte("INITIAL")
	tail := []*document.Delta{
		{DocumentID: uuid.New(), Seq: 1, Payload: []byte("UPDATE-1")},
		{DocumentID: uuid.New(), Seq: 2, Payload: []byte("U2")},
		{DocumentID: uuid.New(), Seq: 5, Payload: []byte("LAST-DELTA")},
	}
	newState, newSV, upToSeq, err := OpaqueConcatFold(state, []byte("vector"), tail)
	if err != nil {
		t.Fatalf("fold returned error: %v", err)
	}
	if newSV != nil {
		t.Fatalf("expected nil state vector for opaque fold, got %d bytes", len(newSV))
	}
	if upToSeq != 5 {
		t.Fatalf("wrong upToSeq: got %d want 5", upToSeq)
	}

	// Walk the resulting bundle and assert the segments match.
	segments := walkBundle(t, newState)
	want := [][]byte{state, tail[0].Payload, tail[1].Payload, tail[2].Payload}
	if len(segments) != len(want) {
		t.Fatalf("wrong segment count: got %d want %d", len(segments), len(want))
	}
	for i, seg := range segments {
		if !bytes.Equal(seg, want[i]) {
			t.Fatalf("segment %d mismatch: got %x want %x", i, seg, want[i])
		}
	}
}

func TestOpaqueConcatFold_UpToSeqIsLastTailSeq(t *testing.T) {
	// upToSeq must be the highest seq in the tail — the fold caller
	// uses it as the new YStateSeqFloor and any value < the highest
	// tail seq would erase deltas the snapshot hasn't actually
	// absorbed.
	tail := []*document.Delta{
		{Seq: 100, Payload: []byte("a")},
		{Seq: 101, Payload: []byte("b")},
		{Seq: 102, Payload: []byte("c")},
	}
	_, _, upToSeq, err := OpaqueConcatFold(nil, nil, tail)
	if err != nil {
		t.Fatalf("fold returned error: %v", err)
	}
	if upToSeq != 102 {
		t.Fatalf("wrong upToSeq: got %d want 102", upToSeq)
	}
}

func TestFoldFor_StrictZKReturnsNil(t *testing.T) {
	// strict_zk → ServerSnapshotAllowed=false → no fold.
	if FoldFor(Capability{ServerSnapshotAllowed: false}) != nil {
		t.Fatal("expected nil fold for ServerSnapshotAllowed=false")
	}
}

func TestFoldFor_ManagedReturnsOpaqueConcat(t *testing.T) {
	// managed_encrypted today → OpaqueConcatFold (placeholder until
	// YjsMergeFold lands). The test is a structural pin so the
	// future migration is intentional, not accidental.
	f := FoldFor(Capability{ServerSnapshotAllowed: true})
	if f == nil {
		t.Fatal("expected non-nil fold for ServerSnapshotAllowed=true")
	}
	// Sanity check: invoke and assert it produces a length-prefix
	// framed bundle.
	tail := []*document.Delta{{Seq: 1, Payload: []byte("x")}}
	out, _, _, err := f(nil, nil, tail)
	if err != nil {
		t.Fatalf("fold call failed: %v", err)
	}
	if len(out) < 4 {
		t.Fatalf("output too small to contain a length prefix")
	}
}

// walkBundle parses a length-prefixed bundle (as produced by
// OpaqueConcatFold + AssembleSnapshotBundle) into its segments.
func walkBundle(t *testing.T, buf []byte) [][]byte {
	t.Helper()
	segments := make([][]byte, 0)
	for len(buf) > 0 {
		if len(buf) < 4 {
			t.Fatalf("truncated prefix")
		}
		n := binary.BigEndian.Uint32(buf[:4])
		buf = buf[4:]
		if uint32(len(buf)) < n {
			t.Fatalf("truncated segment")
		}
		segments = append(segments, buf[:n])
		buf = buf[n:]
	}
	return segments
}
