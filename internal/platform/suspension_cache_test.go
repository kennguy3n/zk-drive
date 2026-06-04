package platform

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSuspensionCacheReuseAndExpiry covers the basic hit/miss contract:
// a fresh set is served from cache, and the entry stops being served
// once the clock advances past its TTL.
func TestSuspensionCacheReuseAndExpiry(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	c := newSuspensionCache(time.Minute, func() time.Time { return now })

	id := uuid.New()
	c.set(id, true, "abuse")

	st, ok := c.get(id)
	if !ok || !st.suspended || st.reason != "abuse" {
		t.Fatalf("expected fresh hit suspended/abuse, got %+v ok=%v", st, ok)
	}

	now = now.Add(time.Minute + time.Second)
	if _, ok := c.get(id); ok {
		t.Fatalf("expected miss after TTL elapsed")
	}
}

// TestSuspensionCacheDisabled verifies a non-positive TTL disables the
// cache entirely (every lookup misses, set is a no-op).
func TestSuspensionCacheDisabled(t *testing.T) {
	c := newSuspensionCache(0, time.Now)
	id := uuid.New()
	c.set(id, true, "x")
	if _, ok := c.get(id); ok {
		t.Fatalf("disabled cache must never hit")
	}
	if n := c.size(); n != 0 {
		t.Fatalf("disabled cache must not store entries, size=%d", n)
	}
}

// TestSuspensionCacheEvictsExpiredOnGrowth verifies the lazy sweep:
// once the map reaches the sweep threshold, inserting a new key first
// drops entries that have expired, so a long tail of one-off workspace
// ids cannot grow the map without bound.
func TestSuspensionCacheEvictsExpiredOnGrowth(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	c := newSuspensionCache(time.Minute, func() time.Time { return now })
	c.sweepAt = 4 // shrink the threshold so the test stays small

	old := make([]uuid.UUID, c.sweepAt)
	for i := range old {
		old[i] = uuid.New()
		c.set(old[i], false, "")
	}
	if got := c.size(); got != c.sweepAt {
		t.Fatalf("pre-expiry size = %d, want %d", got, c.sweepAt)
	}

	// Advance past the TTL so every existing entry is now expired, then
	// insert a fresh id. That insert reaches the threshold and must
	// trigger a sweep that drops all expired entries.
	now = now.Add(time.Minute + time.Second)
	fresh := uuid.New()
	c.set(fresh, true, "abuse")

	if got := c.size(); got != 1 {
		t.Fatalf("after sweep size = %d, want 1 (only the fresh entry)", got)
	}
	if st, ok := c.get(fresh); !ok || !st.suspended {
		t.Fatalf("fresh entry missing or wrong after sweep: %+v ok=%v", st, ok)
	}
}

// TestSuspensionCacheRefreshDoesNotSweep verifies that re-setting an
// existing key never triggers a sweep (the map size does not grow) and
// that unexpired entries are retained when a sweep does run.
func TestSuspensionCacheRefreshKeepsUnexpired(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	c := newSuspensionCache(time.Minute, func() time.Time { return now })
	c.sweepAt = 3

	// Two long-lived entries inserted "now".
	keep := []uuid.UUID{uuid.New(), uuid.New()}
	for _, id := range keep {
		c.set(id, false, "")
	}
	// Advance a little (still within TTL) and add one more, reaching the
	// threshold. Inserting a fresh key triggers a sweep, but nothing is
	// expired yet, so all entries survive.
	now = now.Add(time.Second)
	c.set(keep[0], true, "refresh") // refresh existing -> no growth, no sweep
	fresh := uuid.New()
	c.set(fresh, false, "")

	if got := c.size(); got != 3 {
		t.Fatalf("size = %d, want 3 (nothing expired yet)", got)
	}
	if st, ok := c.get(keep[0]); !ok || !st.suspended || st.reason != "refresh" {
		t.Fatalf("refresh of existing key lost: %+v ok=%v", st, ok)
	}
}
