package platform

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// defaultSuspensionCacheTTL bounds how long a workspace's suspension
// state is reused without re-querying the database. SuspensionGuard
// calls WorkspaceSuspension on every authenticated request across the
// drive / admin / kchat route groups, so an uncached lookup adds a DB
// round-trip to the latency floor of every call. A short TTL collapses
// that to one query per workspace per window while keeping staleness
// small.
//
// The bound is acceptable because suspension is an availability control,
// not a security boundary: SuspendWorkspace also revokes every active
// session via the Redis store (which is observed fleet-wide), and the
// instance that handled the suspend/resume updates its own cache entry
// immediately. The only staleness is a peer instance continuing to
// serve an already-logged-out workspace for at most one TTL.
const defaultSuspensionCacheTTL = 15 * time.Second

// suspensionCacheSweepThreshold is the entry count past which set
// opportunistically evicts expired entries before inserting a new one.
// It bounds the map to roughly the set of workspaces seen within one
// TTL window rather than the all-time distinct count, without needing a
// background goroutine or its own lifecycle.
const suspensionCacheSweepThreshold = 1024

// suspensionState is a cached WorkspaceSuspension result with its
// expiry.
type suspensionState struct {
	suspended bool
	reason    string
	expires   time.Time
}

// suspensionCache is a small TTL cache of per-workspace suspension
// state. A ttl <= 0 disables caching entirely (every lookup misses),
// preserving the always-hit-the-DB behavior as an escape hatch.
type suspensionCache struct {
	mu  sync.RWMutex
	m   map[uuid.UUID]suspensionState
	ttl time.Duration
	now func() time.Time
	// sweepAt is the entry count at which set sweeps expired entries
	// before inserting. Defaults to suspensionCacheSweepThreshold.
	sweepAt int
}

func newSuspensionCache(ttl time.Duration, now func() time.Time) *suspensionCache {
	return &suspensionCache{
		m:       make(map[uuid.UUID]suspensionState),
		ttl:     ttl,
		now:     now,
		sweepAt: suspensionCacheSweepThreshold,
	}
}

// get returns the cached state for id when present and unexpired.
func (c *suspensionCache) get(id uuid.UUID) (suspensionState, bool) {
	if c == nil || c.ttl <= 0 {
		return suspensionState{}, false
	}
	c.mu.RLock()
	st, ok := c.m[id]
	c.mu.RUnlock()
	if !ok || !c.now().Before(st.expires) {
		return suspensionState{}, false
	}
	return st, true
}

// set records the suspension state for id with a fresh TTL.
func (c *suspensionCache) set(id uuid.UUID, suspended bool, reason string) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	// When inserting a *new* key would grow the map past the sweep
	// threshold, drop expired entries first so a long tail of one-off
	// workspace ids cannot grow the map without bound. Amortized cheap:
	// the O(n) scan only runs when the map is already large, and only on
	// inserts that would actually grow it (refreshing an existing key
	// never triggers a sweep).
	if _, exists := c.m[id]; !exists && len(c.m) >= c.sweepAt {
		now := c.now()
		for k, st := range c.m {
			if !now.Before(st.expires) {
				delete(c.m, k)
			}
		}
	}
	c.m[id] = suspensionState{
		suspended: suspended,
		reason:    reason,
		expires:   c.now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// size returns the current number of cached entries (including any not
// yet swept). Used by tests.
func (c *suspensionCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
