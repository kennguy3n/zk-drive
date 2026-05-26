package permission

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// CacheKeyVersion is the version tag baked into every cache key so a
// future change in the resolved-role encoding can be deployed without
// requiring a Redis FLUSHDB on the existing keyspace. A new version
// is a parallel keyspace that the old binary's keys cannot collide
// with — old keys self-expire via TTL.
const CacheKeyVersion = "v1"

// CacheKeyPrefix is the application namespace under which every
// permission cache key lives. Matches the broader ws:* convention
// in internal/session for symmetric grep-ability of Redis keys.
const CacheKeyPrefix = "ws:perm"

// generationStaleAfter bounds how long a replica may serve cache
// reads against a locally-cached generation counter before
// re-fetching from Redis. 500ms is short enough that a
// cross-replica bust propagates within human-perceptible time
// (under 1s end-to-end) while amortising the gen-counter GET
// across the typical request burst (a folder browse fires
// dozens of permission checks within a second).
//
// This is deliberately distinct from the cache-entry TTL itself
// (PerformanceCacheTTL, ~30s). They serve different purposes:
//   - entry TTL: the safety net so stale grants self-expire
//     even when the proactive bust hook misses
//   - gen-staleness: the responsiveness of the proactive bust
//     hook itself
const generationStaleAfter = 500 * time.Millisecond

// CacheObserver is the abstract surface the cache layer depends on
// for emitting observability counters. Defined here (not via the
// internal/metrics package) so internal/permission does not import
// internal/metrics — the dependency flow is metrics-implements-
// observer, not metrics-is-imported-by-everyone.
type CacheObserver interface {
	RecordCacheOp(layer, op, result string)
}

// noopCacheObserver makes nil-safety a single-site concern instead
// of leaking into every call site. The zero value implements
// CacheObserver and does nothing.
type noopCacheObserver struct{}

func (noopCacheObserver) RecordCacheOp(string, string, string) {}

// CachedRepository decorates a Repository with a Redis-backed
// read-through cache for the two access-check methods
// (CheckAccess and CheckAccessWithInheritance). All other methods
// pass through to the delegate. Cache invalidation is driven by
// the workspace-scoped generation counter — every Grant / Revoke /
// BustWorkspace call increments the counter, which atomically
// invalidates every cached entry for that workspace without an
// O(N) SCAN over the keyspace.
//
// Negative caching is symmetric to positive caching: a deny
// outcome is stored under the same TTL as an allow outcome.
// Without this, a probing attacker who repeatedly requests an
// unauthorised resource would force a full Postgres
// CheckAccessWithInheritance evaluation on every request, turning
// auth failures into a per-attacker DoS vector against the DB.
//
// Fail-open semantics: any Redis-side error (timeout, connection
// refused, malformed response) is logged at debug level and the
// call falls through to the delegate. The cache is a perf
// accelerator — it MUST NOT introduce availability risk. The
// fail-open posture is the explicit corollary.
//
// Concurrency: safe for concurrent use. The generation counter is
// read under a sync.RWMutex; cache entries are individual Redis
// keys with atomic operations.
type CachedRepository struct {
	delegate Repository
	rdb      redis.UniversalClient
	ttl      time.Duration
	obs      CacheObserver

	// gen caches the per-workspace generation counter so the
	// common case (cache HIT after the gen has been observed
	// at least once in the last generationStaleAfter window)
	// is a single Redis GET on the entry key, not two GETs.
	// A bust on any replica INCRs the Redis counter; the next
	// read on this replica observes the new value within
	// generationStaleAfter.
	genMu sync.RWMutex
	gen   map[uuid.UUID]*generationEntry
}

// generationEntry caches the workspace generation counter with a
// short-lived freshness window. Stored under genMu via a pointer
// so the struct fields can be re-stamped atomically under the
// write lock.
type generationEntry struct {
	value     int64
	fetchedAt time.Time
}

// NewCachedRepository returns a CachedRepository wrapping
// delegate. rdb is the Redis client used for cache storage; ttl
// is the per-entry expiry written on every cache fill; obs may
// be nil (a no-op observer is substituted).
//
// The constructor accepts the dependencies explicitly rather than
// constructing them from environment variables so the wiring is
// testable: cache_test.go injects a miniredis-backed client and
// a recording observer.
func NewCachedRepository(delegate Repository, rdb redis.UniversalClient, ttl time.Duration, obs CacheObserver) *CachedRepository {
	if obs == nil {
		obs = noopCacheObserver{}
	}
	return &CachedRepository{
		delegate: delegate,
		rdb:      rdb,
		ttl:      ttl,
		obs:      obs,
		gen:      make(map[uuid.UUID]*generationEntry),
	}
}

// Underlying returns the wrapped repository. Used by tests and by
// the service-level invalidation path that wants to issue a
// best-effort BustWorkspace without going through the cache layer
// itself.
func (c *CachedRepository) Underlying() Repository { return c.delegate }

// generationKey returns the per-workspace generation counter key.
// The counter is incremented (via INCR) on every BustWorkspace
// call; read keys embed the current value so an INCR atomically
// invalidates every entry written under the previous value
// without an O(N) DEL sweep.
func generationKey(workspaceID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:gen:%s", CacheKeyPrefix, CacheKeyVersion, workspaceID.String())
}

// entryKey returns the cache key for a single access-check
// result. The shape matches the contract documented at the top of
// the file:
//
//	ws:perm:v1:{workspaceID}:gen{generation}:{resourceType}:{resourceID}:{granteeType}:{granteeID}:{kind}:{minRole}
//
// kind disambiguates flat vs. inheritance checks because their
// resolved-role semantics differ (most-specific-wins for
// inheritance; max-of-direct for flat) and a single key serving
// both would be incorrect.
func entryKey(workspaceID uuid.UUID, generation int64, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, kind, minRole string) string {
	return fmt.Sprintf("%s:%s:%s:gen%d:%s:%s:%s:%s:%s:%s",
		CacheKeyPrefix, CacheKeyVersion,
		workspaceID.String(),
		generation,
		resourceType,
		resourceID.String(),
		granteeType,
		granteeID.String(),
		kind,
		minRole,
	)
}

// cache value sentinel strings. Strings (not 0/1) so a corrupted
// Redis value can be distinguished from a legitimate "0" cache
// hit and fall through to the delegate rather than mis-rendering
// as a deny.
const (
	cacheValueAllow = "a"
	cacheValueDeny  = "d"

	cacheKindFlat        = "f"
	cacheKindInheritance = "i"
)

// loadGeneration returns the current generation counter for the
// workspace. The value is cached locally for generationStaleAfter
// to amortise the round-trip across a burst of permission checks.
// A fresh cache miss triggers a single Redis GET; on Redis error
// the function returns 0 along with the error so the caller can
// degrade gracefully (treat as a miss, skip the cache).
func (c *CachedRepository) loadGeneration(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	c.genMu.RLock()
	entry, ok := c.gen[workspaceID]
	c.genMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < generationStaleAfter {
		return entry.value, nil
	}
	raw, err := c.rdb.Get(ctx, generationKey(workspaceID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, fmt.Errorf("read generation: %w", err)
	}
	var gen int64
	if err == nil {
		parsed, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			// Treat unparseable counter as the safest
			// possible response — invalidate everything by
			// rolling to a fresh value the binary writes
			// next. We do NOT correct the counter here
			// because two replicas racing to "fix" a
			// corrupted counter would just thrash; an
			// operator's psql DEL is the safer recovery
			// path. Log so the corruption surfaces.
			logging.FromContext(ctx).Warn("permission cache: unparseable generation counter; bypassing cache",
				"workspace_id", workspaceID,
				"raw", raw,
				"err", perr,
			)
			return 0, fmt.Errorf("parse generation: %w", perr)
		}
		gen = parsed
	}
	c.genMu.Lock()
	c.gen[workspaceID] = &generationEntry{value: gen, fetchedAt: time.Now()}
	c.genMu.Unlock()
	return gen, nil
}

// CheckAccess implements the flat access check with a
// read-through Redis cache. On HIT we resolve the cached
// allow/deny without touching Postgres; on MISS we delegate to
// the underlying repository and cache the result for ttl. On any
// Redis error we fail open: the call still serves the correct
// answer (from the delegate) but the cache layer is bypassed.
func (c *CachedRepository) CheckAccess(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	return c.cachedAccessCheck(ctx, cacheKindFlat, workspaceID, resourceType, resourceID, granteeType, granteeID, minRole,
		c.delegate.CheckAccess,
	)
}

// CheckAccessWithInheritance implements the ancestor-walking
// access check with a read-through Redis cache. Same caching
// strategy as CheckAccess; the cache key embeds a distinct kind
// tag (`i` vs `f`) so the two resolution semantics never share
// an entry.
func (c *CachedRepository) CheckAccessWithInheritance(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	return c.cachedAccessCheck(ctx, cacheKindInheritance, workspaceID, resourceType, resourceID, granteeType, granteeID, minRole,
		c.delegate.CheckAccessWithInheritance,
	)
}

// accessCheckFn is the shared signature of the two delegate
// methods. Threading the delegate in as a function pointer lets
// CheckAccess and CheckAccessWithInheritance share the cache
// logic without inheriting a switch statement on cacheKind*.
type accessCheckFn func(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error)

// cachedAccessCheck is the read-through cache implementation
// shared by both check methods. The flow:
//
//  1. Load the workspace generation (locally cached for 500ms).
//  2. Compose the entry key using that generation.
//  3. GET the entry. On HIT, return immediately (DB untouched).
//  4. On MISS (or Redis error), delegate to the underlying repo.
//  5. SET the resolved value under ttl. Fail-soft on SET error.
//
// Validation (isValidRole etc.) is intentionally NOT duplicated
// here — the underlying PostgresRepository.CheckAccess[...]
// methods validate minRole already, and replicating the check
// would let a future divergence in validation semantics manifest
// at the cache layer first. Cache-miss path runs the same
// validation; cache-hit path is only reachable AFTER a previous
// miss already passed validation.
func (c *CachedRepository) cachedAccessCheck(ctx context.Context, kind string, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string, fn accessCheckFn) (bool, error) {
	gen, genErr := c.loadGeneration(ctx, workspaceID)
	if genErr != nil {
		c.obs.RecordCacheOp(layerPerm, opRead, resultError)
		return fn(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, minRole)
	}
	key := entryKey(workspaceID, gen, resourceType, resourceID, granteeType, granteeID, kind, minRole)

	val, err := c.rdb.Get(ctx, key).Result()
	switch {
	case err == nil:
		// Cache hit — decode and return without touching the
		// delegate. Sentinel-string parsing means an
		// out-of-band corruption (someone SET an arbitrary
		// string) falls through to the delegate instead of
		// returning a wrong allow/deny.
		switch val {
		case cacheValueAllow:
			c.obs.RecordCacheOp(layerPerm, opRead, resultHit)
			return true, nil
		case cacheValueDeny:
			c.obs.RecordCacheOp(layerPerm, opRead, resultNegativeHit)
			return false, nil
		default:
			logging.FromContext(ctx).Warn("permission cache: unexpected entry value; bypassing",
				"key", key,
				"value", val,
			)
			c.obs.RecordCacheOp(layerPerm, opRead, resultError)
		}
	case errors.Is(err, redis.Nil):
		// Cache miss — fall through to the delegate.
		c.obs.RecordCacheOp(layerPerm, opRead, resultMiss)
	default:
		// Redis-side failure — log and fail open.
		logging.FromContext(ctx).Debug("permission cache: read failed; bypassing",
			"err", err,
			"key", key,
		)
		c.obs.RecordCacheOp(layerPerm, opRead, resultError)
		// Still try the delegate; do not store the result
		// (the SET would likely fail too and the noise isn't
		// useful).
		return fn(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, minRole)
	}

	allowed, derr := fn(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, minRole)
	if derr != nil {
		// Don't cache errors — a transient DB blip should
		// not poison the cache.
		return false, derr
	}
	stored := cacheValueDeny
	if allowed {
		stored = cacheValueAllow
	}
	if setErr := c.rdb.Set(ctx, key, stored, c.ttl).Err(); setErr != nil {
		logging.FromContext(ctx).Debug("permission cache: write failed; serving uncached",
			"err", setErr,
			"key", key,
		)
		c.obs.RecordCacheOp(layerPerm, opWrite, resultError)
	} else {
		c.obs.RecordCacheOp(layerPerm, opWrite, resultOK)
	}
	return allowed, nil
}

// Create wraps the underlying repository's grant insertion and
// then busts the workspace cache. We bust BEFORE returning so a
// subsequent CheckAccess call in the same request (e.g. a UI that
// optimistically re-checks after a Grant) sees the new state.
// Bust failure is best-effort: the grant is durable in Postgres
// and the cache entries will self-expire via TTL.
func (c *CachedRepository) Create(ctx context.Context, p *Permission) error {
	if err := c.delegate.Create(ctx, p); err != nil {
		return err
	}
	c.bustWorkspace(ctx, p.WorkspaceID)
	return nil
}

// Delete wraps the underlying repository's grant removal and
// then busts the workspace cache. Same bust-before-return
// rationale as Create — a UI that re-checks after a Revoke must
// see the deny outcome immediately.
func (c *CachedRepository) Delete(ctx context.Context, workspaceID, permID uuid.UUID) error {
	if err := c.delegate.Delete(ctx, workspaceID, permID); err != nil {
		return err
	}
	c.bustWorkspace(ctx, workspaceID)
	return nil
}

// GetByID passes through to the delegate. The result of a
// single-row GetByID is not worth caching — it has no hot-path
// callers in the current codebase, and adding it would inflate
// the per-workspace cache key cardinality without a measurable
// win.
func (c *CachedRepository) GetByID(ctx context.Context, workspaceID, permID uuid.UUID) (*Permission, error) {
	return c.delegate.GetByID(ctx, workspaceID, permID)
}

// ListByResource passes through to the delegate. The list
// endpoints are not on the per-request hot path (they're called
// when an admin opens a sharing dialog, not on every file
// access).
func (c *CachedRepository) ListByResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]*Permission, error) {
	return c.delegate.ListByResource(ctx, workspaceID, resourceType, resourceID)
}

// ListByGrantee passes through to the delegate. Same rationale
// as ListByResource.
func (c *CachedRepository) ListByGrantee(ctx context.Context, workspaceID uuid.UUID, granteeType string, granteeID uuid.UUID) ([]*Permission, error) {
	return c.delegate.ListByGrantee(ctx, workspaceID, granteeType, granteeID)
}

// BustWorkspace invalidates every cached access-check result for
// a workspace. Called by the service layer when a non-permission
// mutation (folder move, folder delete, file move) changes the
// ancestry chain — in which case any cached
// CheckAccessWithInheritance result for a descendant is now
// potentially stale.
//
// Implementation is a single INCR on the per-workspace
// generation counter. Old entries become unreachable
// (subsequent reads compose their key with the new generation)
// and self-expire via TTL.
func (c *CachedRepository) BustWorkspace(ctx context.Context, workspaceID uuid.UUID) {
	c.bustWorkspace(ctx, workspaceID)
}

func (c *CachedRepository) bustWorkspace(ctx context.Context, workspaceID uuid.UUID) {
	gen, err := c.rdb.Incr(ctx, generationKey(workspaceID)).Result()
	if err != nil {
		logging.FromContext(ctx).Warn("permission cache: bust failed; relying on TTL",
			"workspace_id", workspaceID,
			"err", err,
		)
		c.obs.RecordCacheOp(layerPerm, opBust, resultError)
		return
	}
	// The generation counter has no TTL.
	//
	// An earlier version expired the counter at 2× the entry
	// TTL on the theory that it would self-clean for quiet
	// workspaces. That permits a narrow but real stale-read
	// race: if read traffic continues (refreshing entries via
	// SET on every miss) but no busts happen, the counter
	// expires while entries from gen=N are still alive. A
	// subsequent INCR sets the counter back to 1 (Redis INCR
	// on a missing key yields 1), and surviving entries
	// originally written under gen=1 become reachable again
	// for the remainder of their TTL. Removing the counter
	// TTL eliminates the race entirely.
	//
	// Memory cost: one int64 per workspace. At 10k workspaces
	// that's ~60KB of Redis state — negligible. Workspaces
	// that are deleted should DEL the counter as part of
	// their teardown (out of scope for this PR; the cache
	// safely ignores leaked counters because the keyspace is
	// keyed by workspaceID).
	// Update the local cache so subsequent reads on THIS
	// replica observe the new generation immediately. Other
	// replicas pick up the new value within
	// generationStaleAfter.
	c.genMu.Lock()
	c.gen[workspaceID] = &generationEntry{value: gen, fetchedAt: time.Now()}
	c.genMu.Unlock()
	c.obs.RecordCacheOp(layerPerm, opBust, resultBust)
}

// Cache-op label constants. Defined here (not via the
// internal/metrics package) so internal/permission stays free of
// an internal/metrics import — see CacheObserver doc above.
const (
	layerPerm         = "perm"
	opRead            = "read"
	opWrite           = "write"
	opBust            = "bust"
	resultHit         = "hit"
	resultMiss        = "miss"
	resultNegativeHit = "negative_hit"
	resultBust        = "bust"
	resultError       = "error"
	resultOK          = "ok"
)
