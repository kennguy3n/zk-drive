package preview

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/jobs"
)

// tierCacheKeyPrefix namespaces the per-workspace tier cache keys.
// The full key is preview_tier:{workspace_id} holding the tier string
// with a short TTL.
const tierCacheKeyPrefix = "preview_tier:"

// DefaultTierCacheTTL is how long a resolved workspace tier is cached
// in Redis before the next lookup re-reads workspace_plans. Five
// minutes keeps a Stripe-driven tier change visible to the preview
// router within a few minutes while collapsing the per-job tier
// lookup to a single Redis GET for the overwhelmingly common case of
// a workspace uploading many files in a burst.
const DefaultTierCacheTTL = 5 * time.Minute

// TierCache resolves a workspace's billing tier (workspace_plans.tier)
// with a short-lived Redis cache in front of Postgres. It is used by
// the preview pipeline to (a) label the budget-exceeded metric by
// bounded tier and (b) decide the NATS priority subject a preview job
// should be dispatched on.
//
// A workspace with no workspace_plans row resolves to billing.TierFree
// — the same "new workspace defaults to free" convention the billing
// service uses.
//
// A nil *TierCache is a valid receiver whose Tier always returns
// billing.TierFree: when Redis/Postgres wiring is absent the pipeline
// degrades to treating every tenant as standard-tier, which is the
// safe (non-prioritising) default.
type TierCache struct {
	pool *pgxpool.Pool
	rdb  redis.UniversalClient
	ttl  time.Duration
}

// NewTierCache builds a tier resolver over pool with an optional Redis
// cache. rdb may be nil (every lookup hits Postgres directly); ttl <=
// 0 falls back to DefaultTierCacheTTL.
func NewTierCache(pool *pgxpool.Pool, rdb redis.UniversalClient, ttl time.Duration) *TierCache {
	if ttl <= 0 {
		ttl = DefaultTierCacheTTL
	}
	return &TierCache{pool: pool, rdb: rdb, ttl: ttl}
}

// Tier returns the billing tier for workspaceID. The cache is
// read-through: a Redis hit short-circuits the DB; a miss queries
// workspace_plans and back-fills Redis. Redis-side errors are
// non-fatal — the lookup falls through to Postgres (fail-open on the
// cache, never on the source of truth).
func (c *TierCache) Tier(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	if c == nil || c.pool == nil {
		return billing.TierFree, nil
	}
	key := tierCacheKeyPrefix + workspaceID.String()
	if c.rdb != nil {
		if v, err := c.rdb.Get(ctx, key).Result(); err == nil && v != "" {
			return v, nil
		}
	}
	tier, err := c.queryTier(ctx, workspaceID)
	if err != nil {
		return billing.TierFree, err
	}
	if c.rdb != nil {
		// Best-effort cache fill; a Redis write failure must not
		// fail an otherwise-successful tier resolution.
		_ = c.rdb.Set(ctx, key, tier, c.ttl).Err()
	}
	return tier, nil
}

func (c *TierCache) queryTier(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	const q = `SELECT tier FROM workspace_plans WHERE workspace_id = $1`
	var tier string
	if err := c.pool.QueryRow(ctx, q, workspaceID).Scan(&tier); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return billing.TierFree, nil
		}
		return "", fmt.Errorf("load workspace tier: %w", err)
	}
	if tier == "" {
		return billing.TierFree, nil
	}
	return tier, nil
}

// PreviewSubject maps a workspace to the NATS subject its preview jobs
// should be published on, so the Business / Secure-Business tiers land
// on the priority subject and everyone else on the standard subject.
// Errors degrade to the standard subject — a tier-lookup blip must
// never drop a preview job. The API-side dispatcher uses this to pick
// jobs.PublishPreviewTier's subject from a workspace id.
func (c *TierCache) PreviewSubject(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	tier, err := c.Tier(ctx, workspaceID)
	if err != nil {
		return jobs.SubjectPreviewStandard, err
	}
	return jobs.PreviewSubjectForTier(tier), nil
}
