package middleware

import (
	"context"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// redisRateLimitWindow is the bucket width used for the sliding
// window counter. A 1s window gives the same per-second budget as the
// in-memory token bucket without requiring fractional counters and
// keeps Redis hot keys short-lived (each window key is auto-expired).
const redisRateLimitWindow = time.Second

// rateLimitScript runs the user / workspace counter checks
// atomically on the Redis server. It mirrors the in-memory
// limiter's two-phase logic:
//
//  1. INCR user counter and EXPIRE it. If it exceeds the user
//     budget, return immediately — the workspace counter is *not*
//     touched. This prevents a single misbehaving client from
//     inflating the workspace counter with denied requests and
//     starving every other user in the workspace (Devin Review
//     #3150549270).
//  2. INCR workspace counter and EXPIRE it. If it exceeds the
//     workspace budget, DECR the user counter (refund) and signal
//     a workspace denial.
//  3. Otherwise allow the request.
//
// Return shape: {status, user_count, ws_count} where status is
// 0=allowed, 1=user-denied, 2=workspace-denied.
var rateLimitScript = redis.NewScript(`
local user_key = KEYS[1]
local ws_key = KEYS[2]
local user_rate = tonumber(ARGV[1])
local ws_rate = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local has_ws = ARGV[4] == "1"

local u = redis.call('INCR', user_key)
redis.call('EXPIRE', user_key, ttl)
if u > user_rate then
  return {1, u, 0}
end

if has_ws then
  local w = redis.call('INCR', ws_key)
  redis.call('EXPIRE', ws_key, ttl)
  if w > ws_rate then
    redis.call('DECR', user_key)
    return {2, u - 1, w}
  end
  return {0, u, w}
end

return {0, u, 0}
`)

// RedisRateLimiterConfig matches the in-memory RateLimitConfig so the
// caller can swap implementations without changing wiring.
type RedisRateLimiterConfig struct {
	PerUser      int
	PerWorkspace int
}

// RedisRateLimiter returns an http middleware that enforces sliding
// window rate limits using Redis INCR + EXPIRE. The middleware shares
// state across replicas by keying counters on
// rl:{workspaceID}:{userID}:{window} where {window} is the unix
// second the request falls into.
//
// The middleware is fail-open: if Redis is unreachable we log and
// allow the request through. Rate limiting is a best-effort defence
// against accidental floods, not a security control — silently
// blocking every request because Redis hiccuped would be a worse
// outcome than briefly serving above quota.
func RedisRateLimiter(client redis.UniversalClient, cfg RedisRateLimiterConfig) func(http.Handler) http.Handler {
	userRate := cfg.PerUser
	if userRate <= 0 {
		userRate = DefaultUserRate
	}
	wsRate := cfg.PerWorkspace
	if wsRate <= 0 {
		wsRate = DefaultWorkspaceRate
	}
	limiter := &redisRateLimiter{
		client:   client,
		userRate: userRate,
		wsRate:   wsRate,
		window:   redisRateLimitWindow,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := UserIDFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			workspaceID, _ := WorkspaceIDFromContext(r.Context())
			wait := limiter.reserve(r.Context(), workspaceID, userID, time.Now())
			if wait > 0 {
				retry := int(math.Ceil(wait.Seconds()))
				if retry < 1 {
					retry = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type redisRateLimiter struct {
	client   redis.UniversalClient
	userRate int
	wsRate   int
	window   time.Duration
}

// reserve runs rateLimitScript to atomically check and increment the
// per-user and per-workspace counters. Returns the wait interval the
// caller must respect before retrying; 0 means the request is
// allowed.
//
// The script's two-phase logic is what guarantees that a denied
// per-user request does not pollute the workspace counter, so a
// single user can't 429 the entire workspace by ignoring 429s. The
// in-memory limiter has the same property via tokenBucket.refund —
// see ratelimit.go:reserve.
//
// EXPIRE is set to 2× the window inside the script so a counter
// that wraps to a new window before the old one expires can never
// accumulate stale values from the previous window. Doing it inside
// the script also means there's a single network round-trip rather
// than the two-step pipeline the previous implementation used.
func (l *redisRateLimiter) reserve(ctx context.Context, workspaceID, userID uuid.UUID, now time.Time) time.Duration {
	bucket := now.Truncate(l.window).Unix()
	userKey := rateLimitKey(workspaceID, userID, bucket)
	wsKey := workspaceLimitKey(workspaceID, bucket)
	ttlSeconds := int64((2 * l.window).Seconds())
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	hasWS := "0"
	if workspaceID != uuid.Nil {
		hasWS = "1"
	}

	res, err := rateLimitScript.Run(ctx, l.client,
		[]string{userKey, wsKey},
		l.userRate, l.wsRate, ttlSeconds, hasWS,
	).Slice()
	if err != nil {
		// Fail open. Logging here is intentional — a quiet drop
		// would mask Redis outages from operators.
		log.Printf("ratelimit_redis: script failed, allowing request: %v", err)
		return 0
	}
	if len(res) < 1 {
		log.Printf("ratelimit_redis: script returned no values, allowing request")
		return 0
	}
	status, _ := res[0].(int64)
	if status == 0 {
		return 0
	}

	bucketEnd := time.Unix(bucket, 0).Add(l.window)
	wait := time.Until(bucketEnd)
	if wait < 0 {
		wait = 0
	}
	return wait + time.Millisecond
}

// rateLimitKey returns the per-user counter key. The (empty)
// workspace ID still gets a slot so workspace-less users don't
// collide with each other.
func rateLimitKey(workspaceID, userID uuid.UUID, window int64) string {
	return "rl:" + workspaceID.String() + ":" + userID.String() + ":" + strconv.FormatInt(window, 10)
}

func workspaceLimitKey(workspaceID uuid.UUID, window int64) string {
	return "rl:ws:" + workspaceID.String() + ":" + strconv.FormatInt(window, 10)
}

