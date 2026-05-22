package health

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// PostgresChecker probes a pgxpool via its native Ping which acquires
// a connection from the pool and issues `SELECT 1`. A pool-level
// failure (no free connections, db unreachable, auth rejected) is
// surfaced as the check error.
type PostgresChecker struct {
	pool *pgxpool.Pool
}

// NewPostgresChecker constructs a Checker that reports under
// Name() == "postgres".
func NewPostgresChecker(pool *pgxpool.Pool) *PostgresChecker {
	return &PostgresChecker{pool: pool}
}

// Name implements Checker.
func (p *PostgresChecker) Name() string { return "postgres" }

// Check implements Checker.
func (p *PostgresChecker) Check(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return errors.New("postgres pool not initialised")
	}
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	return nil
}

// RedisChecker probes a *redis.Client via PING. A nil client is
// reported as "not configured" rather than failing — this matches
// the optional-redis posture of the rest of the stack (REDIS_URL
// unset == in-memory single-process mode, which is a valid dev
// deployment, not a not-ready state).
type RedisChecker struct {
	client *redis.Client
}

// NewRedisChecker wraps a *redis.Client. Pass nil to indicate the
// dependency is intentionally absent — the resulting checker will
// report "not configured" rather than fail.
func NewRedisChecker(client *redis.Client) *RedisChecker {
	return &RedisChecker{client: client}
}

// Name implements Checker.
func (r *RedisChecker) Name() string { return "redis" }

// Check implements Checker. Returns nil when redis is absent (by
// design) or when PING succeeds; non-nil only on a genuine outage.
func (r *RedisChecker) Check(ctx context.Context) error {
	if r == nil || r.client == nil {
		// Optional dependency, intentionally absent.
		return nil
	}
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}

// storageProbe is the contract the storage health-check depends on
// — declared as an unexported interface so tests in this package can
// substitute a fake without pulling the AWS SDK into the test binary.
// External callers go through NewStorageChecker, which takes the
// concrete *storage.Client type to avoid Go's typed-nil interface
// trap: passing a nil *storage.Client into a storageProbe parameter
// would yield a non-nil interface value wrapping a nil pointer, which
// would silently skip the nil short-circuit in Check.
type storageProbe interface {
	HealthCheck(ctx context.Context) error
}

// StorageChecker probes a storage backend via HealthCheck. A nil
// underlying probe is treated as "storage not configured" (returns
// OK) so dev stacks without an S3 gateway don't fail readiness.
//
// HealthCheck on *storage.Client issues an S3 HeadBucket call. The
// configured credentials therefore need the s3:ListBucket permission
// on the bucket — a presign-only IAM scope (s3:GetObject /
// s3:PutObject) is insufficient and will cause /readyz to always
// return 503 with a 403 in the response. When using AWS IAM, attach
// a policy granting s3:ListBucket on the bucket ARN; the equivalent
// in Ceph RGW / MinIO is the bucket-level READ capability.
type StorageChecker struct {
	probe storageProbe
}

// NewStorageChecker wraps the internal/storage.Client. It deliberately
// accepts the concrete *storage.Client type — not the storageProbe
// interface — so a nil pointer (the common path when S3 is not
// configured) is normalised to a nil interface field inside the
// constructor. Passing a nil *storage.Client directly to a parameter
// of interface type would otherwise produce the canonical
// non-nil-interface-wrapping-nil-pointer footgun ("typed nil"), and
// the Check method's `s.probe == nil` short-circuit would fail to
// fire — /readyz would then 503 in every deployment without S3.
//
// Pass nil to indicate "storage intentionally absent"; the resulting
// checker reports OK on Check.
func NewStorageChecker(client *storage.Client) *StorageChecker {
	if client == nil {
		// Explicit nil interface assignment so probe == nil holds.
		return &StorageChecker{probe: nil}
	}
	return &StorageChecker{probe: client}
}

// Name implements Checker.
func (s *StorageChecker) Name() string { return "storage" }

// Check implements Checker.
func (s *StorageChecker) Check(ctx context.Context) error {
	if s == nil || s.probe == nil {
		return nil
	}
	if err := s.probe.HealthCheck(ctx); err != nil {
		return fmt.Errorf("storage health: %w", err)
	}
	return nil
}

// NATSChecker probes a *nats.Conn. The check is the lightweight
// Status() inspection rather than a synchronous Flush, because
// Flush would round-trip to the server with no extra signal vs the
// connection-state machine and could block the readiness probe
// beyond its timeout under intermittent network conditions.
type NATSChecker struct {
	conn *nats.Conn
}

// NewNATSChecker wraps a *nats.Conn. Pass nil for "NATS not
// configured"; this is the common path in dev stacks.
func NewNATSChecker(conn *nats.Conn) *NATSChecker {
	return &NATSChecker{conn: conn}
}

// Name implements Checker.
func (n *NATSChecker) Name() string { return "nats" }

// Check implements Checker. Treats CONNECTED / RECONNECTING as OK;
// CLOSED / DISCONNECTED / DRAINING / unknown as not ready.
func (n *NATSChecker) Check(_ context.Context) error {
	if n == nil || n.conn == nil {
		return nil
	}
	switch s := n.conn.Status(); s {
	case nats.CONNECTED, nats.RECONNECTING:
		return nil
	default:
		return fmt.Errorf("nats status %s", s)
	}
}
