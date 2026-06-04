package middleware

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"

	"github.com/redis/go-redis/v9"
)

// DefaultPlatformIPRate is the fallback per-client-IP request budget
// (requests per second) for the IP limiter. It is generous for a real
// fleet operator driving the platform control plane while still capping
// an unauthenticated spray of bogus API keys.
const DefaultPlatformIPRate = 20

// ipRateLimitScript is the single-counter sliding-window check for the
// per-IP limiter, mirroring rateLimitScript's INCR+EXPIRE pattern.
// Returns {status, count} where status is 0=allowed, 1=denied.
var ipRateLimitScript = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local c = redis.call('INCR', key)
redis.call('EXPIRE', key, ttl)
if c > rate then
  return {1, c}
end
return {0, c}
`)

// IPRateLimiter returns a middleware that applies a per-client-IP
// request rate limit. It exists for route groups that run OUTSIDE the
// workspace/user JWT scope — e.g. the platform control plane, which is
// authenticated only by a platform API key and therefore can't be keyed
// by the per-user/-workspace limiter. It is defense-in-depth against an
// unauthenticated request flood (a spray of bogus `pk_` tokens), not a
// correctness control.
//
// Like the per-user limiter it is fail-OPEN: a Redis outage logs and
// allows the request rather than locking operators out of the control
// plane. When client is nil it falls back to a per-replica in-memory
// token bucket (matching how the main limiter degrades without Redis).
//
// The client IP is resolved via ClientIPFromRequest honouring
// trustedProxyDepth, so a spoofed X-Forwarded-For can't be used to
// dodge the limit (the same extraction the IP allowlist uses).
func IPRateLimiter(client redis.UniversalClient, perIP, trustedProxyDepth int) func(http.Handler) http.Handler {
	if perIP <= 0 {
		perIP = DefaultPlatformIPRate
	}
	l := &ipRateLimiter{
		client:            client,
		rate:              perIP,
		window:            redisRateLimitWindow,
		trustedProxyDepth: trustedProxyDepth,
	}
	if client == nil {
		l.mem = newIPMemoryLimiter(float64(perIP))
		go l.mem.runJanitor(5 * time.Minute)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIPFromRequest(r, l.trustedProxyDepth)
			key := "unknown"
			if ip != nil {
				key = ip.String()
			}
			if wait := l.reserve(r.Context(), key, time.Now()); wait > 0 {
				retry := int(math.Ceil(wait.Seconds()))
				if retry < 1 {
					retry = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				RespondError(w, http.StatusTooManyRequests, ErrCodeRateLimit, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type ipRateLimiter struct {
	client            redis.UniversalClient
	rate              int
	window            time.Duration
	trustedProxyDepth int
	mem               *ipMemoryLimiter
}

// reserve returns the wait interval the caller must respect before
// retrying; 0 means allowed. Uses Redis when configured (shared across
// replicas), otherwise the in-memory fallback.
func (l *ipRateLimiter) reserve(ctx context.Context, ip string, now time.Time) time.Duration {
	if l.client == nil {
		return l.mem.reserve(ip, now)
	}
	bucket := now.Truncate(l.window).Unix()
	key := "iprl:" + ip + ":" + strconv.FormatInt(bucket, 10)
	ttlSeconds := int64((2 * l.window).Seconds())
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	res, err := ipRateLimitScript.Run(ctx, l.client, []string{key}, l.rate, ttlSeconds).Slice()
	if err != nil {
		// Fail open (see the per-user limiter): never lock the control
		// plane out because Redis hiccuped.
		logging.FromContext(ctx).Error("ratelimit_ip script failed, allowing request", "err", err)
		return 0
	}
	if len(res) < 1 {
		logging.FromContext(ctx).Warn("ratelimit_ip script returned no values, allowing request")
		return 0
	}
	if status, _ := res[0].(int64); status == 0 {
		return 0
	}
	bucketEnd := time.Unix(bucket, 0).Add(l.window)
	wait := time.Until(bucketEnd)
	if wait < 0 {
		wait = 0
	}
	return wait + time.Millisecond
}

// ipMemoryLimiter is the per-replica in-memory fallback used when Redis
// is not configured. It reuses tokenBucket keyed by client IP string.
type ipMemoryLimiter struct {
	rate float64
	cap  float64

	mu      sync.Mutex
	buckets map[string]*tokenBucket
	touched map[string]time.Time
}

func newIPMemoryLimiter(rate float64) *ipMemoryLimiter {
	return &ipMemoryLimiter{
		rate:    rate,
		cap:     rate * burstMultiplier,
		buckets: make(map[string]*tokenBucket),
		touched: make(map[string]time.Time),
	}
}

func (l *ipMemoryLimiter) reserve(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	b := l.buckets[ip]
	if b == nil {
		b = &tokenBucket{tokens: l.cap, capacity: l.cap, rate: l.rate, lastRefill: now}
		l.buckets[ip] = b
	}
	l.touched[ip] = now
	l.mu.Unlock()
	return b.consume(now)
}

func (l *ipMemoryLimiter) runJanitor(window time.Duration) {
	t := time.NewTicker(window)
	defer t.Stop()
	for now := range t.C {
		threshold := now.Add(-2 * window)
		l.mu.Lock()
		for ip, last := range l.touched {
			if last.After(threshold) {
				continue
			}
			delete(l.touched, ip)
			delete(l.buckets, ip)
		}
		l.mu.Unlock()
	}
}
