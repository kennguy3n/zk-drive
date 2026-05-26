package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Cache layer labels. Closed set — adding a new cache layer means
// adding a constant here, NOT free-form label strings at the call
// site. Today only the permission cache exists (CacheLayerPerm);
// the listing cache lands in a follow-up PR after we measure
// permission-cache hit rate in production.
const (
	// CacheLayerPerm is the permission resolution read-through
	// cache implemented in internal/permission.CachedRepository.
	CacheLayerPerm = "perm"
)

// Cache operation labels. Today only the read path emits the op
// label; bust events use CacheOpBust on either layer. Keeping op
// as a separate label (vs. baking layer:op into the layer label)
// keeps cardinality bounded at 1 + N for N op types — every new
// layer reuses the same op vocabulary.
const (
	// CacheOpRead is the read-through lookup the cache layer
	// performs on every CheckAccessWithInheritance / similar
	// call. Result distinguishes hit / miss / negative_hit /
	// error.
	CacheOpRead = "read"
	// CacheOpBust is the write path emitted when the proactive
	// invalidation hook fires (permission grant / revoke,
	// folder move / delete). Result distinguishes ok / error;
	// hit/miss are not meaningful for a bust.
	CacheOpBust = "bust"
	// CacheOpWrite is the cache-fill emitted right after a
	// repository miss, when the cache layer stores the
	// resolved value back into Redis. Result distinguishes
	// ok / error; tracking this separately from CacheOpRead
	// lets operators alert on cache-fill failures without
	// noise from the (expected) miss path.
	CacheOpWrite = "write"
)

// Cache result labels. Bounded vocabulary:
//   - hit:          positive-result cache hit (returned without DB)
//   - miss:         no cache entry, fell through to delegate
//   - negative_hit: cached deny / not-found served without DB
//   - bust:         invalidation succeeded (counter only, no hit/miss)
//   - ok:           cache-fill (CacheOpWrite) succeeded. Paired with
//                   CacheResultError on the same op to let operators
//                   alert on cache-fill failures distinct from read
//                   misses.
//   - error:        Redis returned an error other than redis.Nil; the
//                   call fell through to the delegate (fail open)
const (
	CacheResultHit         = "hit"
	CacheResultMiss        = "miss"
	CacheResultNegativeHit = "negative_hit"
	CacheResultBust        = "bust"
	CacheResultOK          = "ok"
	CacheResultError       = "error"
)

// RecordCacheOp emits the cache-ops counter for a single layer +
// op + result tuple. Nil-safe so the no-metrics boot mode pays one
// nil-check.
//
// Implements the abstract recorder surface
// (internal/permission.CacheObserver) so packages can record
// metrics without importing this package directly.
func (m *Metrics) RecordCacheOp(layer, op, result string) {
	if m == nil || m.cacheOpsTotal == nil {
		return
	}
	m.cacheOpsTotal.WithLabelValues(layer, op, result).Inc()
}

// CacheObserver is the minimal surface a cache layer depends on
// to record its observability counter. Defined here so consumers
// can spell out the dependency without importing the full metrics
// package — and so unit tests can supply a recording fake.
type CacheObserver interface {
	RecordCacheOp(layer, op, result string)
}

// registerCacheMetrics mounts the cache-ops counter on the
// supplied registry. Same promauto.With(reg) pattern as every
// other metric family in metrics.New().
func (m *Metrics) registerCacheMetrics(reg prometheus.Registerer) {
	auto := promauto.With(reg)

	m.cacheOpsTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_cache_ops_total",
		Help: "Total operations against the application-level caches in front of Postgres, partitioned by layer ('perm' = permission resolution cache; listing caches arrive in a follow-up), op ('read' = lookup attempt; 'write' = cache-fill after miss; 'bust' = proactive invalidation), and result ('hit' = positive cache hit; 'miss' = no entry, delegated to repo; 'negative_hit' = cached negative result served without DB; 'bust' = invalidation succeeded; 'error' = Redis-side failure, fell through to repo).",
	}, []string{"layer", "op", "result"})
}
