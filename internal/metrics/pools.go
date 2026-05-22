package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// RegisterPgxPoolCollector adds a Collector to the Metrics
// registry that emits live pgxpool.Stat values on each scrape.
// The stats are pulled lazily (pool.Stat() is called inside
// Collect, not at registration time) so a hot-swap of the pool
// is not supported — but the pool's identity is stable for the
// process lifetime, so that's fine.
//
// Emitted series (all gauges, no labels):
//   - zkdrive_db_pool_total_conns        — current pool size
//   - zkdrive_db_pool_acquired_conns     — currently checked-out
//   - zkdrive_db_pool_idle_conns         — checked-in
//   - zkdrive_db_pool_max_conns          — configured MaxConns
//   - zkdrive_db_pool_acquire_count      — counter (cumulative)
//   - zkdrive_db_pool_acquire_duration_seconds — counter (cumulative)
//
// A nil pool is a no-op so cold-start setups without Postgres
// (rare; only the migrate binary skips this) don't crash.
func (m *Metrics) RegisterPgxPoolCollector(pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	m.Registry.MustRegister(&pgxPoolCollector{pool: pool})
}

type pgxPoolCollector struct {
	pool *pgxpool.Pool
}

// Descriptors for the pgxpool collector. Kept as package-level
// vars so the prometheus.Desc objects are allocated once per
// process rather than per scrape.
var (
	pgxPoolTotalConnsDesc = prometheus.NewDesc(
		"zkdrive_db_pool_total_conns",
		"Total number of connections currently in the pgxpool (idle + acquired + constructing).",
		nil, nil,
	)
	pgxPoolAcquiredConnsDesc = prometheus.NewDesc(
		"zkdrive_db_pool_acquired_conns",
		"Number of pgxpool connections currently checked out to callers.",
		nil, nil,
	)
	pgxPoolIdleConnsDesc = prometheus.NewDesc(
		"zkdrive_db_pool_idle_conns",
		"Number of pgxpool connections currently idle (checked in, ready to lease).",
		nil, nil,
	)
	pgxPoolMaxConnsDesc = prometheus.NewDesc(
		"zkdrive_db_pool_max_conns",
		"Configured maximum size of the pgxpool (MaxConns).",
		nil, nil,
	)
	pgxPoolAcquireCountDesc = prometheus.NewDesc(
		"zkdrive_db_pool_acquire_count",
		"Cumulative number of successful Acquire() calls served by the pgxpool.",
		nil, nil,
	)
	pgxPoolAcquireDurationDesc = prometheus.NewDesc(
		"zkdrive_db_pool_acquire_duration_seconds",
		"Cumulative time (seconds) spent inside Acquire() waiting for a connection to free up.",
		nil, nil,
	)
)

// Describe implements prometheus.Collector.
func (c *pgxPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- pgxPoolTotalConnsDesc
	ch <- pgxPoolAcquiredConnsDesc
	ch <- pgxPoolIdleConnsDesc
	ch <- pgxPoolMaxConnsDesc
	ch <- pgxPoolAcquireCountDesc
	ch <- pgxPoolAcquireDurationDesc
}

// Collect implements prometheus.Collector. Called once per scrape.
// pool.Stat() returns a snapshot of the current pool state; it's
// cheap (a handful of atomic loads) so doing it on every scrape
// is fine.
func (c *pgxPoolCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(pgxPoolTotalConnsDesc, prometheus.GaugeValue, float64(stat.TotalConns()))
	ch <- prometheus.MustNewConstMetric(pgxPoolAcquiredConnsDesc, prometheus.GaugeValue, float64(stat.AcquiredConns()))
	ch <- prometheus.MustNewConstMetric(pgxPoolIdleConnsDesc, prometheus.GaugeValue, float64(stat.IdleConns()))
	ch <- prometheus.MustNewConstMetric(pgxPoolMaxConnsDesc, prometheus.GaugeValue, float64(stat.MaxConns()))
	ch <- prometheus.MustNewConstMetric(pgxPoolAcquireCountDesc, prometheus.CounterValue, float64(stat.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(pgxPoolAcquireDurationDesc, prometheus.CounterValue, stat.AcquireDuration().Seconds())
}

// RegisterRedisPoolCollector adds a Collector to the Metrics
// registry that emits redis.Client.PoolStats() on each scrape.
// A nil client is a no-op so deployments that opt out of Redis
// (REDIS_URL unset → in-memory rate limiter + session store)
// don't crash.
//
// Emitted series:
//   - zkdrive_redis_pool_total_conns   — current pool size
//   - zkdrive_redis_pool_idle_conns    — currently idle
//   - zkdrive_redis_pool_stale_conns   — closed for staleness
//   - zkdrive_redis_pool_hits          — counter (cumulative)
//   - zkdrive_redis_pool_misses        — counter (cumulative)
//   - zkdrive_redis_pool_timeouts      — counter (cumulative)
func (m *Metrics) RegisterRedisPoolCollector(client *redis.Client) {
	if client == nil {
		return
	}
	m.Registry.MustRegister(&redisPoolCollector{client: client})
}

type redisPoolCollector struct {
	client *redis.Client
}

var (
	redisPoolTotalConnsDesc = prometheus.NewDesc(
		"zkdrive_redis_pool_total_conns",
		"Total number of connections currently in the redis client pool.",
		nil, nil,
	)
	redisPoolIdleConnsDesc = prometheus.NewDesc(
		"zkdrive_redis_pool_idle_conns",
		"Number of redis connections currently idle in the pool.",
		nil, nil,
	)
	redisPoolStaleConnsDesc = prometheus.NewDesc(
		"zkdrive_redis_pool_stale_conns",
		"Number of redis connections closed for staleness (cumulative).",
		nil, nil,
	)
	redisPoolHitsDesc = prometheus.NewDesc(
		"zkdrive_redis_pool_hits",
		"Cumulative number of times the redis pool returned an existing idle connection (hit) on Get().",
		nil, nil,
	)
	redisPoolMissesDesc = prometheus.NewDesc(
		"zkdrive_redis_pool_misses",
		"Cumulative number of times the redis pool had to dial a fresh connection (miss) on Get().",
		nil, nil,
	)
	redisPoolTimeoutsDesc = prometheus.NewDesc(
		"zkdrive_redis_pool_timeouts",
		"Cumulative number of times a redis pool Get() timed out waiting for a free connection.",
		nil, nil,
	)
)

func (c *redisPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- redisPoolTotalConnsDesc
	ch <- redisPoolIdleConnsDesc
	ch <- redisPoolStaleConnsDesc
	ch <- redisPoolHitsDesc
	ch <- redisPoolMissesDesc
	ch <- redisPoolTimeoutsDesc
}

func (c *redisPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.client.PoolStats()
	if s == nil {
		// PoolStats can technically be nil right after Close().
		// Emit nothing; the scraper will see the series go
		// missing, which is the correct "process gone" signal.
		return
	}
	ch <- prometheus.MustNewConstMetric(redisPoolTotalConnsDesc, prometheus.GaugeValue, float64(s.TotalConns))
	ch <- prometheus.MustNewConstMetric(redisPoolIdleConnsDesc, prometheus.GaugeValue, float64(s.IdleConns))
	ch <- prometheus.MustNewConstMetric(redisPoolStaleConnsDesc, prometheus.CounterValue, float64(s.StaleConns))
	ch <- prometheus.MustNewConstMetric(redisPoolHitsDesc, prometheus.CounterValue, float64(s.Hits))
	ch <- prometheus.MustNewConstMetric(redisPoolMissesDesc, prometheus.CounterValue, float64(s.Misses))
	ch <- prometheus.MustNewConstMetric(redisPoolTimeoutsDesc, prometheus.CounterValue, float64(s.Timeouts))
}
