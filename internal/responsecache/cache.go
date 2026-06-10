// Package responsecache is a small, fail-open, Redis-backed
// read-through cache for expensive *read* responses (folder listings
// with permission resolution, storage-usage aggregation, search
// results) at 5000-tenant + B2C scale.
//
// It deliberately mirrors the invalidation discipline already proven in
// internal/permission/cache.go: every cached entry is namespaced under a
// per-workspace *generation counter*, so a single INCR on that counter
// atomically invalidates every entry for the workspace without an O(N)
// SCAN/DEL sweep of the keyspace. Entries also carry a TTL as a safety
// net so a missed bust self-heals.
//
// Fail-open is the cardinal rule: the cache is a latency accelerator,
// never a source of truth. Any Redis error (timeout, refused, malformed
// value) is logged at debug and the caller's compute function runs as if
// the cache were absent. A nil *Cache is valid and behaves as a
// permanent miss, so callers wire it unconditionally and the absence of
// REDIS_URL simply disables caching.
package responsecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// KeyVersion is baked into every key so the encoding of a cached value
// can change in a future release without a Redis flush: a new version is
// a parallel keyspace the old binary's keys cannot collide with, and the
// stale keys self-expire via TTL.
const KeyVersion = "v1"

// KeyPrefix namespaces every response-cache key. Distinct from the
// ws:perm prefix used by the permission cache so the two keyspaces are
// independently greppable and bustable.
const KeyPrefix = "ws:resp"

// generationStaleAfter bounds how long a replica serves reads against
// its locally-cached copy of a workspace's generation counter before
// re-reading it from Redis. A cross-replica bust therefore becomes
// visible on every replica within this window. 500ms matches the
// permission cache: short enough that a stale listing self-corrects
// sub-second, long enough to amortise the counter GET across the burst
// of cache lookups a single page render fires.
const generationStaleAfter = 500 * time.Millisecond

// Cache is a workspace-scoped, generation-invalidated read-through
// cache. The zero value is not usable; construct with New. A nil *Cache
// is valid and behaves as a permanent miss (see package doc).
type Cache struct {
	rdb redis.UniversalClient

	genMu sync.RWMutex
	gen   map[uuid.UUID]*generationEntry
}

type generationEntry struct {
	value     int64
	fetchedAt time.Time
}

// New returns a Cache backed by rdb. Passing a nil rdb returns a nil
// *Cache so callers can write
//
//	cache := responsecache.New(redisClient) // redisClient may be nil
//
// and get a no-op cache when Redis is unconfigured.
func New(rdb redis.UniversalClient) *Cache {
	if rdb == nil {
		return nil
	}
	return &Cache{
		rdb: rdb,
		gen: make(map[uuid.UUID]*generationEntry),
	}
}

// Enabled reports whether c will actually talk to Redis. False for a nil
// receiver or a Cache built without a client.
func (c *Cache) Enabled() bool { return c != nil && c.rdb != nil }

func generationKey(workspaceID uuid.UUID) string {
	return fmt.Sprintf("%s:%s:gen:%s", KeyPrefix, KeyVersion, workspaceID.String())
}

// entryKey composes the per-entry key. domain separates the three
// caches (folder/usage/search) so their keyspaces never collide; key is
// the caller-supplied, domain-local discriminator (e.g. the folder id +
// pagination, or a normalised search query).
func entryKey(workspaceID uuid.UUID, generation int64, domain, key string) string {
	return fmt.Sprintf("%s:%s:%s:gen%d:%s:%s",
		KeyPrefix, KeyVersion, workspaceID.String(), generation, domain, key)
}

// loadGeneration returns the workspace's current generation counter,
// caching it locally for generationStaleAfter to amortise the round-trip
// across a burst of lookups. On Redis error it returns the error so the
// caller bypasses the cache (fail-open). Mirrors the monotonic-write
// race handling documented in internal/permission/cache.go.
func (c *Cache) loadGeneration(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
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
			logging.FromContext(ctx).Warn("response cache: unparseable generation counter; bypassing cache",
				"workspace_id", workspaceID, "raw", raw, "err", perr)
			return 0, fmt.Errorf("parse generation: %w", perr)
		}
		gen = parsed
	}
	c.genMu.Lock()
	if existing, ok := c.gen[workspaceID]; !ok || gen >= existing.value {
		c.gen[workspaceID] = &generationEntry{value: gen, fetchedAt: time.Now()}
	} else {
		gen = existing.value
	}
	c.genMu.Unlock()
	return gen, nil
}

// BustWorkspace invalidates every cached entry for the workspace by
// incrementing its generation counter. Safe to call on a nil/!Enabled
// cache (no-op). Best-effort: a Redis failure is logged and swallowed so
// a bust never fails the mutation that triggered it — the entry TTL is
// the backstop. The local generation copy is refreshed on the next read
// via the generationStaleAfter window.
func (c *Cache) BustWorkspace(ctx context.Context, workspaceID uuid.UUID) {
	if !c.Enabled() {
		return
	}
	if err := c.rdb.Incr(ctx, generationKey(workspaceID)).Err(); err != nil {
		logging.FromContext(ctx).Debug("response cache: bust failed (entries will expire via TTL)",
			"workspace_id", workspaceID, "err", err)
		return
	}
	// Expire the local copy so this replica re-reads the bumped counter
	// immediately rather than serving stale for up to generationStaleAfter.
	c.genMu.Lock()
	delete(c.gen, workspaceID)
	c.genMu.Unlock()
}

// GetOrCompute is the read-through entry point. On a cache HIT it returns
// the decoded cached value; on MISS (or any Redis/JSON error) it invokes
// compute, caches a successful result under ttl, and returns it. compute
// errors are propagated and never cached.
//
// T is encoded as JSON. Use it for the response DTOs these caches serve;
// it is not meant for arbitrary binary blobs.
func GetOrCompute[T any](ctx context.Context, c *Cache, workspaceID uuid.UUID, domain, key string, ttl time.Duration, compute func(context.Context) (T, error)) (T, error) {
	var zero T
	if !c.Enabled() {
		return compute(ctx)
	}
	gen, err := c.loadGeneration(ctx, workspaceID)
	if err != nil {
		// Fail open: generation unreadable, behave as uncached.
		return compute(ctx)
	}
	ek := entryKey(workspaceID, gen, domain, key)
	if raw, gerr := c.rdb.Get(ctx, ek).Bytes(); gerr == nil {
		var v T
		if uerr := json.Unmarshal(raw, &v); uerr == nil {
			return v, nil
		}
		// Corrupt entry: fall through to recompute + overwrite. Log so
		// persistent corruption is visible.
		logging.FromContext(ctx).Debug("response cache: unmarshal failed; recomputing", "key", ek)
	} else if !errors.Is(gerr, redis.Nil) {
		// Redis error (not a plain miss): fail open without caching.
		logging.FromContext(ctx).Debug("response cache: get failed; bypassing", "key", ek, "err", gerr)
		return compute(ctx)
	}

	v, err := compute(ctx)
	if err != nil {
		return zero, err
	}
	if payload, merr := json.Marshal(v); merr == nil {
		if serr := c.rdb.Set(ctx, ek, payload, ttl).Err(); serr != nil {
			logging.FromContext(ctx).Debug("response cache: set failed", "key", ek, "err", serr)
		}
	}
	return v, nil
}
