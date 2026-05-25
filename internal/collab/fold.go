package collab

import (
	"errors"

	"github.com/kennguy3n/zk-drive/internal/document"
)

// OpaqueConcatFold implements document.FoldFunc by concatenating
// the current y_state and each tail-delta payload with a 4-byte
// length-prefix per segment. The resulting blob is a valid
// "concatenated update stream" that the client side splits on the
// length prefixes and feeds segment-by-segment into Y.applyUpdate.
//
// This is the PERMANENT fold strategy for strict_zk folders: the
// server cannot decrypt the payloads, so a real Y.mergeUpdates is
// impossible. The y_state grows monotonically over time; clients
// pay the apply-update cost on cold open but never see the
// individual delta tail (it's been folded into y_state and
// trimmed).
//
// For managed_encrypted folders, OpaqueConcatFold is a TEMPORARY
// placeholder until a Yjs WASM (or CGo) bridge ships in a follow-
// up PR. The bridge will provide a YjsMergeFold that produces a
// compact single-update y_state via Y.mergeUpdates. Replacing this
// implementation with the merge fold is a drop-in swap — the
// FoldFunc signature is unchanged and the client side is no-op
// (apply-update on a single optimal update vs a sequence of
// length-prefixed updates is equivalent from the editor's point
// of view).
//
// Output:
//   - newState:       length-prefix(currentState) || length-prefix(tail[0].Payload) || ... || length-prefix(tail[N-1].Payload)
//   - newStateVector: nil — the opaque fold cannot compute a Yjs
//     state vector without parsing the payload. Clients that
//     need a state vector reconstruct it locally by applying
//     the bundle and calling Y.encodeStateVector. The HTTP
//     layer's snapshot endpoint omits state_vector when nil.
//   - upToSeq:        tail[len(tail)-1].Seq — the seq of the
//     last delta folded, which becomes the document's new
//     y_state_seq_floor via ReplaceSnapshot.
//
// The fold returns an error when the tail is empty: an empty fold
// would attempt to ReplaceSnapshot with no progress, which the
// service refuses with a "non-progressing upToSeq" error. The
// caller (the hub's compaction scheduler) MUST check for an empty
// tail before invoking.
//
// The fold is pure (no I/O), so it's safe to call from any
// goroutine without locking.
func OpaqueConcatFold(currentState, _currentStateVector []byte, tail []*document.Delta) ([]byte, []byte, int64, error) {
	if len(tail) == 0 {
		return nil, nil, 0, errors.New("collab: OpaqueConcatFold called with empty tail")
	}

	// Pre-compute the total length so we allocate exactly once.
	total := 4 + len(currentState)
	for _, d := range tail {
		total += 4 + len(d.Payload)
	}

	out := make([]byte, 0, total)
	out = append(out, LengthPrefix(currentState)...)
	for _, d := range tail {
		out = append(out, LengthPrefix(d.Payload)...)
	}
	// upToSeq is the seq of the LAST delta folded; tail is
	// returned by GetSnapshotBundle ordered by seq ASC so the
	// final element is the highest seq.
	upToSeq := tail[len(tail)-1].Seq
	return out, nil, upToSeq, nil
}

// FoldFor returns the appropriate FoldFunc for a folder's
// capability. Today both managed_encrypted and strict_zk route to
// OpaqueConcatFold; when YjsMergeFold ships this switch grows
// a case for ServerSnapshotAllowed=true so managed_encrypted gets
// the optimized merge path.
//
// Returns nil when ServerSnapshotAllowed is false on a folder
// whose policy explicitly forbids server-side snapshot work — the
// caller should skip compaction entirely for such rooms (no fold
// runs). Today strict_zk is the only such mode but the gating is
// kept policy-driven so future modes (e.g. a PublicDistribution
// collab feature) inherit the same behaviour without code change.
//
// We don't read folder.EncryptionMode here because the hub already
// resolved capability at connect time — passing the bool through
// avoids a second lookup AND avoids importing the folder package
// from the fold layer.
func FoldFor(cap Capability) document.FoldFunc {
	if !cap.ServerSnapshotAllowed {
		// strict_zk: while OpaqueConcatFold technically works
		// (it doesn't decrypt), the resulting compaction does
		// not save space — y_state grows by exactly the sum of
		// tail payloads. The hub policy is "don't auto-compact
		// strict_zk rooms"; an admin can still force compaction
		// via a future endpoint.
		return nil
	}
	return OpaqueConcatFold
}
