package preview

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// DefaultBudgetPerWorkspaceHour is the fallback per-workspace preview
// budget when the operator has not configured
// PREVIEW_BUDGET_PER_WORKSPACE_HOUR. One hundred previews/hour is
// generous for interactive use (a user opening a folder triggers a
// burst of thumbnails) yet low enough that a single tenant
// bulk-uploading thousands of files cannot monopolise the worker
// fleet and starve every other tenant's previews.
const DefaultBudgetPerWorkspaceHour = 100

// DefaultBudgetWindow is the sliding window the budget is measured
// over. The task specifies a 1-hour window; it is a field on
// TenantPreviewBudget rather than a hard constant so tests can use a
// short window without sleeping for an hour.
const DefaultBudgetWindow = time.Hour

// budgetBackoffBase is the first redelivery delay applied to a
// budget-deferred preview job; subsequent deferrals double it up to
// MaxBudgetBackoff. Sized so a workspace that briefly bursts past its
// budget retries quickly (the window is constantly draining) while a
// workspace pinned at its ceiling backs off to the 5-minute cap
// rather than hot-looping the consumer.
const budgetBackoffBase = 15 * time.Second

// MaxBudgetBackoff caps the exponential redelivery delay for a
// budget-deferred preview job, per the task's "exponential backoff up
// to 5 minutes" requirement.
const MaxBudgetBackoff = 5 * time.Minute

// QueueMaxDeliver is the JetStream redelivery cap for the priority /
// standard preview consumers. It is intentionally higher than
// MaxDeliver (the poison-payload cap on the legacy consumer) because
// these consumers' Naks include budget DEFERRALS, not just decode
// failures: a workspace pinned at its ceiling for the full 1-hour
// window can legitimately be redelivered ~15-20 times (backoff ramps
// to the 5-minute cap), and capping at MaxDeliver=5 would drop those
// previews instead of eventually rendering them. A genuinely poison
// payload still terminates — just after more attempts, costing only
// bounded extra log noise.
const QueueMaxDeliver = 50

// BudgetBackoff returns the redelivery delay for a budget-deferred
// preview job on its numDelivered-th delivery attempt (1-based, as
// reported by nats msg metadata). The delay doubles each attempt from
// budgetBackoffBase and saturates at MaxBudgetBackoff.
func BudgetBackoff(numDelivered int) time.Duration {
	if numDelivered < 1 {
		numDelivered = 1
	}
	d := budgetBackoffBase
	for i := 1; i < numDelivered; i++ {
		d *= 2
		if d >= MaxBudgetBackoff {
			return MaxBudgetBackoff
		}
	}
	if d > MaxBudgetBackoff {
		return MaxBudgetBackoff
	}
	return d
}

// budgetKeyPrefix namespaces the sliding-window counter keys. Mirrors
// the `ws:`-style key conventions used by internal/session and
// internal/permission so all of zk-drive's Redis keys remain
// grep-able by tenant scope. The full key is
//
//	preview_budget:{workspace_id}
//
// holding a Redis sorted set whose members are unique per admitted
// preview and whose scores are the admission timestamp (unix
// milliseconds).
const budgetKeyPrefix = "preview_budget:"

// BudgetDecision is the outcome of a single TenantPreviewBudget.Allow
// call. Count is the number of previews already admitted in the
// current window (after the just-admitted one when Allowed is true,
// or the rejecting count when Allowed is false), and Limit echoes the
// configured ceiling so callers can log "got X of Y" without re-
// reading config.
type BudgetDecision struct {
	Allowed bool
	Count   int
	Limit   int
}

// TenantPreviewBudget enforces a per-workspace preview rate limit
// using a Redis-backed sliding-window log. Each admitted preview adds
// a uniquely-scored member to a per-workspace sorted set; admission
// is denied once the number of members whose score falls inside the
// trailing window reaches the limit.
//
// The sliding-window log (rather than a fixed-window INCR) avoids the
// classic burst-at-the-boundary problem where a tenant can fire 2×
// the limit across two adjacent fixed windows. The trade-off is O(N)
// members stored per active workspace, bounded by the limit itself
// (expired members are trimmed on every Allow and the key carries a
// window-length TTL so an idle workspace's set self-cleans).
//
// A nil *TenantPreviewBudget is a valid no-op receiver: Allow always
// admits. This mirrors the nil-safe Publisher pattern so the worker
// behaves identically when Redis is not configured (single-replica /
// local dev) without every call site null-checking.
type TenantPreviewBudget struct {
	rdb    redis.UniversalClient
	limit  int
	window time.Duration
	now    func() time.Time
}

// NewTenantPreviewBudget builds a budget enforcer backed by rdb. A nil
// rdb (Redis not configured) yields a nil enforcer so Allow degrades
// to "always admit" — the budget is a fairness guard, not a
// correctness guard, and must never take down preview generation when
// Redis is unavailable. limit <= 0 falls back to
// DefaultBudgetPerWorkspaceHour; window <= 0 falls back to
// DefaultBudgetWindow.
func NewTenantPreviewBudget(rdb redis.UniversalClient, limit int, window time.Duration) *TenantPreviewBudget {
	if rdb == nil {
		return nil
	}
	if limit <= 0 {
		limit = DefaultBudgetPerWorkspaceHour
	}
	if window <= 0 {
		window = DefaultBudgetWindow
	}
	return &TenantPreviewBudget{
		rdb:    rdb,
		limit:  limit,
		window: window,
		now:    time.Now,
	}
}

// Limit returns the configured per-window ceiling. Exposed so callers
// (and the worker's exceeded-budget log line) can report the limit
// without reaching into the struct.
func (b *TenantPreviewBudget) Limit() int {
	if b == nil {
		return 0
	}
	return b.limit
}

// budgetScript is the atomic sliding-window admission check. Splitting
// trim / count / conditional-add across separate round trips would
// race: two workers could both observe count == limit-1 and both
// admit, overshooting the budget. Running the whole decision as one
// Lua script makes it atomic on the Redis server.
//
//	KEYS[1] = preview_budget:{workspace_id}
//	ARGV[1] = now (unix ms)
//	ARGV[2] = window length (ms)
//	ARGV[3] = limit
//	ARGV[4] = unique member id for this admission
//
// Returns {allowed, count} where allowed is 1/0 and count is the
// window-resident admission count (post-add when admitted).
var budgetScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now - window)
local count = redis.call('ZCARD', KEYS[1])
if count >= limit then
	return {0, count}
end
redis.call('ZADD', KEYS[1], now, ARGV[4])
redis.call('PEXPIRE', KEYS[1], window)
return {1, count + 1}
`)

// Allow attempts to admit one preview for workspaceID against the
// sliding window. It returns Allowed=true (and records the admission)
// when the workspace is under budget, Allowed=false otherwise.
//
// A nil receiver always admits. Redis errors are surfaced to the
// caller (the worker logs and admits — fail-open) rather than
// swallowed here, so the decision policy lives at the call site
// alongside the NATS ack semantics.
func (b *TenantPreviewBudget) Allow(ctx context.Context, workspaceID uuid.UUID) (BudgetDecision, error) {
	if b == nil {
		return BudgetDecision{Allowed: true}, nil
	}
	now := b.now()
	member := fmt.Sprintf("%d-%s", now.UnixNano(), uuid.NewString())
	res, err := budgetScript.Run(ctx, b.rdb,
		[]string{budgetKey(workspaceID)},
		now.UnixMilli(),
		b.window.Milliseconds(),
		b.limit,
		member,
	).Result()
	if err != nil {
		return BudgetDecision{Limit: b.limit}, fmt.Errorf("preview budget check: %w", err)
	}
	allowed, count := parseBudgetResult(res)
	return BudgetDecision{Allowed: allowed, Count: count, Limit: b.limit}, nil
}

// parseBudgetResult decodes the {allowed, count} array the Lua script
// returns. go-redis surfaces a Lua table as []interface{} of int64.
// Defensive against an unexpected shape (returns "denied" so a
// malformed reply never silently admits past the budget).
func parseBudgetResult(res any) (bool, int) {
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return false, 0
	}
	allowed, _ := arr[0].(int64)
	count, _ := arr[1].(int64)
	return allowed == 1, int(count)
}

func budgetKey(workspaceID uuid.UUID) string {
	return budgetKeyPrefix + workspaceID.String()
}
