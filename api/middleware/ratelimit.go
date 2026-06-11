package middleware

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultUserRate is the fallback token-bucket fill rate (requests
// per second) applied when the config value is unset or non-positive.
const DefaultUserRate = 100

// DefaultWorkspaceRate is the fallback per-workspace fill rate
// (requests per second).
const DefaultWorkspaceRate = 1000

// burstMultiplier scales the bucket capacity relative to the fill
// rate. Bursts of up to 2× the steady-state rate are acceptable for
// interactive workloads where users occasionally open many tabs at
// once.
const burstMultiplier = 2

// RateLimitConfig is the user-facing surface of the rate limiter.
// Zero values fall back to the exported defaults so a misconfigured
// env var never silently disables limiting.
type RateLimitConfig struct {
	PerUser      int
	PerWorkspace int
}

// RateLimiter returns a middleware that applies token-bucket rate
// limiting keyed by (workspace_id, user_id). Requests without a
// resolved user id pass through untouched — the auth middleware is
// expected to run first, and unauthenticated traffic is handled by
// whichever upstream gateway terminates TLS for us.
//
// The middleware is safe for concurrent use: buckets live in a
// sync.Map and each bucket holds its own mutex so hot accounts never
// block cold ones. Idle buckets are cleaned up lazily by a background
// goroutine.
func RateLimiter(ctx context.Context, cfg RateLimitConfig) func(http.Handler) http.Handler {
	userRate := cfg.PerUser
	if userRate <= 0 {
		userRate = DefaultUserRate
	}
	wsRate := cfg.PerWorkspace
	if wsRate <= 0 {
		wsRate = DefaultWorkspaceRate
	}
	limiter := newRateLimiter(float64(userRate), float64(wsRate))
	go limiter.runJanitor(ctx, 5*time.Minute)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := UserIDFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			workspaceID, _ := WorkspaceIDFromContext(r.Context())
			res := limiter.reserve(workspaceID, userID)
			// Report the configured steady-state per-user rate as the
			// limit, identical to the Redis limiter
			// (ratelimit_redis.go) so X-RateLimit-Limit means the same
			// thing — the operator's RATE_LIMIT_PER_USER — no matter
			// which backend is active. The token bucket's 2x burst
			// capacity stays an internal grace: setRateLimitHeaders
			// clamps Remaining to the limit so the advertised budget
			// never over-promises what a sustained client may send.
			setRateLimitHeaders(w, int(limiter.userRate), res.remaining, res.reset)
			if res.wait > 0 {
				retry := int(math.Ceil(res.wait.Seconds()))
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

type rateLimiter struct {
	userRate float64 // tokens/sec
	userCap  float64

	wsRate float64
	wsCap  float64

	mu      sync.Mutex
	users   map[uuid.UUID]*tokenBucket
	work    map[uuid.UUID]*tokenBucket
	touched map[uuid.UUID]time.Time
}

func newRateLimiter(userRate, wsRate float64) *rateLimiter {
	return &rateLimiter{
		userRate: userRate,
		userCap:  userRate * burstMultiplier,
		wsRate:   wsRate,
		wsCap:    wsRate * burstMultiplier,
		users:    make(map[uuid.UUID]*tokenBucket),
		work:     make(map[uuid.UUID]*tokenBucket),
		touched:  make(map[uuid.UUID]time.Time),
	}
}

// reserve attempts to consume a token from both the per-user and
// per-workspace buckets. Returns a reservation whose wait is the
// shortest interval the caller must respect before retrying (0 when
// allowed), along with the per-user remaining budget and reset time
// for the X-RateLimit-* headers.
func (l *rateLimiter) reserve(workspaceID, userID uuid.UUID) reservation {
	now := time.Now()
	l.mu.Lock()
	u := l.users[userID]
	if u == nil {
		u = &tokenBucket{tokens: l.userCap, capacity: l.userCap, rate: l.userRate, lastRefill: now}
		l.users[userID] = u
	}
	var ws *tokenBucket
	if workspaceID != uuid.Nil {
		ws = l.work[workspaceID]
		if ws == nil {
			ws = &tokenBucket{tokens: l.wsCap, capacity: l.wsCap, rate: l.wsRate, lastRefill: now}
			l.work[workspaceID] = ws
		}
	}
	l.touched[userID] = now
	if workspaceID != uuid.Nil {
		l.touched[workspaceID] = now
	}
	l.mu.Unlock()

	if wait := u.consume(now); wait > 0 {
		return u.reservation(wait, now)
	}
	if ws != nil {
		if wait := ws.consume(now); wait > 0 {
			// Refund the user token so a workspace-level block does
			// not unfairly drain the user's bucket.
			u.refund()
			return u.reservation(wait, now)
		}
	}
	return u.reservation(0, now)
}

// runJanitor evicts buckets that haven't been touched in twice the
// cleanup window, returning when ctx is cancelled so the goroutine
// does not outlive the server (it is launched once per limiter at
// startup; without ctx it would leak for the process lifetime and,
// worse, accumulate across the many limiters tests construct).
func (l *rateLimiter) runJanitor(ctx context.Context, window time.Duration) {
	t := time.NewTicker(window)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			threshold := now.Add(-2 * window)
			l.mu.Lock()
			for id, last := range l.touched {
				if last.After(threshold) {
					continue
				}
				delete(l.touched, id)
				delete(l.users, id)
				delete(l.work, id)
			}
			l.mu.Unlock()
		}
	}
}

type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	capacity   float64
	rate       float64
	lastRefill time.Time
}

func (b *tokenBucket) consume(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.lastRefill = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	need := 1 - b.tokens
	return time.Duration(need/b.rate*float64(time.Second)) + time.Millisecond
}

func (b *tokenBucket) refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens++
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}

// reservation snapshots the bucket's remaining whole tokens and the
// time it will refill to capacity, for the X-RateLimit-* headers.
// reset is the unix second at which a fully-drained bucket would be
// back at capacity; for a partially-filled bucket it is sooner.
func (b *tokenBucket) reservation(wait time.Duration, now time.Time) reservation {
	b.mu.Lock()
	tokens := b.tokens
	deficit := b.capacity - tokens
	rate := b.rate
	b.mu.Unlock()

	remaining := int(math.Floor(tokens))
	if remaining < 0 {
		remaining = 0
	}
	resetIn := time.Second
	if rate > 0 && deficit > 0 {
		resetIn = time.Duration(deficit / rate * float64(time.Second))
	}
	return reservation{wait: wait, remaining: remaining, reset: now.Add(resetIn).Unix()}
}

// setRateLimitHeaders writes the standard rate-limit telemetry headers
// on every rate-limited response (allowed or throttled) so clients can
// self-pace. X-RateLimit-Limit is the per-window budget,
// X-RateLimit-Remaining the requests left in the current window, and
// X-RateLimit-Reset the unix second the window resets. Must be called
// before the response status is written.
func setRateLimitHeaders(w http.ResponseWriter, limit, remaining int, reset int64) {
	if remaining < 0 {
		remaining = 0
	}
	// Keep the advertised contract honest: Remaining must never
	// exceed Limit. The in-memory token bucket can momentarily hold
	// up to its 2x burst capacity, but we report the steady-state
	// rate as the limit, so clamp the burst surplus down to it.
	if remaining > limit {
		remaining = limit
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
}
