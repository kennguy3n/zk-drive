package collab

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kennguy3n/zk-drive/internal/document"
)

// YjsMergeFold returns a document.FoldFunc that calls into the
// runtime's wasm-backed Yjs merge to produce a compact
// single-update snapshot from the current state + tail of pending
// deltas. Use this for managed_encrypted folders where the server
// has plaintext access — strict_zk routes stay on
// OpaqueConcatFold because the server cannot decrypt to merge.
//
// The returned fold function:
//
//  1. Validates the tail is non-empty (same precondition
//     OpaqueConcatFold enforces).
//  2. Collects (currentState, tail[0].Payload, … tail[N-1].Payload)
//     into the update vector.
//  3. Calls YjsRuntime.MergeUpdates to fold them into a single
//     v1-encoded update.
//  4. Calls YjsRuntime.EncodeStateVector on the merged result
//     to attach the catch-up watermark.
//  5. Returns (merged, stateVector, lastTailSeq, nil).
//
// On wasm error (parse/decode/apply failure), the fold function
// returns an error. The caller (the hub's compaction scheduler)
// logs and skips that compaction — the document remains in its
// uncompacted state, which is correct: a failed fold MUST NOT
// trim the tail.
//
// Lifetime: the returned closure captures `rt` by reference. The
// hub schedules it inside a context bound to the server's
// lifecycle, so the runtime must outlive in-flight folds.
// cmd/server/main.go arranges this by deferring runtime.Close
// AFTER hub.Shutdown completes.
//
// A nil runtime panics on use to surface the wiring bug at the
// first compaction rather than silently regressing to opaque
// concat — the caller should pass a real runtime or use FoldFor's
// fallback path. We don't fall back to OpaqueConcatFold inside
// this closure because the two folds produce different output
// shapes (a single compact update vs a length-prefixed bundle)
// and the client-side decoder distinguishes; silently swapping
// at fold time would deliver a bundle to a client expecting a
// compact update.
func YjsMergeFold(rt *YjsRuntime) document.FoldFunc {
	return func(ctx context.Context, currentState, _currentStateVector []byte, tail []*document.Delta) ([]byte, []byte, int64, error) {
		if len(tail) == 0 {
			return nil, nil, 0, errors.New("collab: YjsMergeFold called with empty tail")
		}
		if rt == nil {
			return nil, nil, 0, errors.New("collab: YjsMergeFold called with nil runtime")
		}
		// Assemble the update vector: current state first
		// (skipped when empty so the first compaction doesn't
		// pay a zero-byte decode failure), then each tail
		// payload in seq-ascending order. The yrs apply loop
		// inside the wasm is order-tolerant CRDT-wise but
		// applying in seq order keeps the merge deterministic
		// across replays (same input bytes → same output
		// bytes).
		updates := make([][]byte, 0, 1+len(tail))
		if len(currentState) > 0 {
			updates = append(updates, currentState)
		}
		for _, d := range tail {
			updates = append(updates, d.Payload)
		}

		// ctx is the caller's compaction context — for the
		// production wiring this originates in the hub
		// compaction scheduler, which derives it from the
		// server lifecycle context. Server shutdown cancels
		// every in-flight fold via this ctx, so the
		// wasm-acquire spin loop and any pending wasm call
		// abort within milliseconds instead of blocking
		// graceful shutdown.
		merged, err := rt.MergeUpdates(ctx, updates)
		if err != nil {
			return nil, nil, 0, err
		}

		sv, err := rt.EncodeStateVector(ctx, merged)
		if err != nil {
			// Merge succeeded but SV encode failed. Surfacing
			// the failure as a fold error would force the
			// caller to discard the merged update too —
			// regressing both compaction (the tail stays
			// uncompacted) and snapshot delivery on every
			// subsequent attempt as long as the same input
			// keeps tripping the SV encoder. The merged
			// payload is still useful: clients can apply it
			// and derive their own state vector locally
			// (Yjs's client-side library exposes
			// encodeStateVector), so we proceed with sv=nil
			// and log a warn so operators see the
			// degradation in observability (matches the
			// log surface for the compaction-scheduler's
			// other partial-failure paths in
			// internal/collab/hub.go).
			slog.Default().Warn("collab: ymerge encode_state_vector failed; returning merged update with nil state vector",
				"error", err,
				"merged_bytes", len(merged),
				"tail_len", len(tail),
				"up_to_seq", tail[len(tail)-1].Seq,
			)
			sv = nil
		}

		upToSeq := tail[len(tail)-1].Seq
		return merged, sv, upToSeq, nil
	}
}
