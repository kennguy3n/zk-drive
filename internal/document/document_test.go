package document

import "testing"

// TestDeltaListLimits_HandlerProbeFitsInRepoCap pins the invariant
// referenced by the comment on MaxDeltaPageLimit and by the HTTP
// handler's has_more probe strategy. The handler asks the repo for
// `limit+1` rows to detect end-of-page without a second round-trip;
// if the repo silently clamps that probe to MaxDeltaListLimit, the
// handler can report `has_more=false` while rows still exist beyond
// the page boundary — a subtle correctness bug. Keep
// `MaxDeltaPageLimit + 1 < MaxDeltaListLimit` so the probe is always
// honoured.
func TestDeltaListLimits_HandlerProbeFitsInRepoCap(t *testing.T) {
	if MaxDeltaPageLimit+1 >= MaxDeltaListLimit {
		t.Fatalf("MaxDeltaPageLimit+1 (%d) must be strictly less than MaxDeltaListLimit (%d) so the has_more probe is not silently clamped",
			MaxDeltaPageLimit+1, MaxDeltaListLimit)
	}
}

// TestSnapshotTailFitsInRepoCap pins the matching invariant for the
// Snapshot bundle: the service caller asks for up to
// MaxSnapshotTailDeltas tail rows; the repo's cap must be at least
// that large or a fast-growing document's tail would be silently
// truncated.
func TestSnapshotTailFitsInRepoCap(t *testing.T) {
	if MaxSnapshotTailDeltas > MaxDeltaListLimit {
		t.Fatalf("MaxSnapshotTailDeltas (%d) must be <= MaxDeltaListLimit (%d) so the snapshot bundle's tail is not silently truncated",
			MaxSnapshotTailDeltas, MaxDeltaListLimit)
	}
}
