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

// reserve increments the per-user and per-workspace counters for the
// current window in a single pipeline. Returns the wait interval the
// caller must respect before retrying; 0 means the request is
// allowed.
//
// EXPIRE is set to 2× the window so a counter that wraps to a new
// window before the old one expires can never accumulate stale
// values from the previous window. We don't bother with NX on
// EXPIRE: the cost of resetting the TTL on every increment is
// negligible against the savings of avoiding a separate EXISTS
// round-trip.
func (l *redisRateLimiter) reserve(ctx context.Context, workspaceID, userID uuid.UUID, now time.Time) time.Duration {
	bucket := now.Truncate(l.window).Unix()
	userKey := rateLimitKey(workspaceID, userID, bucket)
	wsKey := workspaceLimitKey(workspaceID, bucket)
	ttl := 2 * l.window

	pipe := l.client.Pipeline()
	userIncr := pipe.Incr(ctx, userKey)
	pipe.Expire(ctx, userKey, ttl)
	var wsIncr *redis.IntCmd
	if workspaceID != uuid.Nil {
		wsIncr = pipe.Incr(ctx, wsKey)
		pipe.Expire(ctx, wsKey, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Fail open. Logging here is intentional — a quiet drop
		// would mask Redis outages from operators.
		log.Printf("ratelimit_redis: pipeline failed, allowing request: %v", err)
		return 0
	}

	bucketEnd := time.Unix(bucket, 0).Add(l.window)
	wait := time.Until(bucketEnd)
	if wait < 0 {
		wait = 0
	}

	if userIncr.Val() > int64(l.userRate) {
		return wait + time.Millisecond
	}
	if wsIncr != nil && wsIncr.Val() > int64(l.wsRate) {
		return wait + time.Millisecond
	}
	return 0
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

