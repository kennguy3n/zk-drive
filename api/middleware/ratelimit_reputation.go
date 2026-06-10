package middleware

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kennguy3n/zk-drive/internal/logging"

	"github.com/redis/go-redis/v9"
)

// Auth brute-force reputation (6.3).
//
// Per-user / per-workspace rate limiting (ratelimit.go) protects an
// authenticated tenant from flooding the API, but it cannot see an
// *unauthenticated* attacker spraying passwords at /login: there is no
// user id to key on until the credentials actually verify. This guard
// closes that gap by tracking failed sign-ins per *client IP* and
// escalating a cooldown the attacker must wait out before the next
// attempt is accepted.
//
// Design notes:
//   - The escalation is a COOLDOWN, not a connection tarpit. A tarpit
//     (sleeping the request for 30s) would hold a goroutine + socket
//     per attempt, which an attacker can weaponise into a cheap
//     resource-exhaustion DoS. Instead we record the failure, compute
//     the next allowed time, and reject early attempts with 429 +
//     Retry-After. Same "1s, 5s, 30s, block" escalation, no held
//     connections.
//   - The IP is resolved via ClientIPFromRequest honouring
//     trustedProxyDepth, so a spoofed X-Forwarded-For cannot dodge the
//     guard (identical extraction to the IP allowlist and the session
//     device fingerprint).
//   - A successful sign-in RESETS the IP's counter, so a legitimate
//     user who eventually types the right password is not punished for
//     earlier typos.
//   - Coarse IP keying means a large NAT (a whole SME office behind one
//     egress IP) shares a reputation. That is why the hard block is
//     deliberately short (default 15m) rather than the full retention
//     window: it must massively slow brute force without locking a
//     shared-NAT office out for hours. The 1s/5s/30s steps are
//     unnoticeable to a human fat-fingering their password.
//   - Fail-OPEN on Redis errors (consistent with the other limiters):
//     a Redis outage must not lock everyone out of sign-in. When Redis
//     is not configured at all, a per-replica in-memory fallback still
//     provides best-effort protection.

const (
	// DefaultAuthFailureThreshold is the number of failed sign-ins
	// from one IP that are tolerated before cooldowns begin. The
	// first DefaultAuthFailureThreshold-1 failures are free (human
	// typos), the threshold-th failure triggers the first cooldown.
	DefaultAuthFailureThreshold = 5

	// DefaultAuthBlockDuration is the hard-block cooldown applied once
	// the progressive delays are exhausted. Kept short so a shared-NAT
	// office is not locked out for hours (see file header).
	DefaultAuthBlockDuration = 15 * time.Minute

	// DefaultAuthReputationRetention is how long an IP's failure
	// counter survives with no further failures. Matches the 6.3
	// requirement ("store IP reputation in Redis with 24h TTL").
	DefaultAuthReputationRetention = 24 * time.Hour
)

// defaultAuthDelays is the progressive cooldown schedule applied at and
// after the failure threshold: the threshold-th failure waits 1s, the
// next 5s, the next 30s; any further failure escalates to the hard
// block (DefaultAuthBlockDuration).
var defaultAuthDelays = []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second}

// AuthReputationConfig tunes the brute-force guard. Zero values fall
// back to the Default* constants so a misconfigured env var never
// silently disables the protection.
type AuthReputationConfig struct {
	FailureThreshold int
	BlockDuration    time.Duration
	Retention        time.Duration
	// Delays is the progressive cooldown schedule. When nil the
	// package default (1s, 5s, 30s) is used.
	Delays []time.Duration
}

func (c AuthReputationConfig) withDefaults() AuthReputationConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = DefaultAuthFailureThreshold
	}
	if c.BlockDuration <= 0 {
		c.BlockDuration = DefaultAuthBlockDuration
	}
	if c.Retention <= 0 {
		c.Retention = DefaultAuthReputationRetention
	}
	if len(c.Delays) == 0 {
		c.Delays = defaultAuthDelays
	}
	return c
}

// AuthReputation tracks failed sign-ins per client IP and computes the
// cooldown an IP must respect before its next attempt is accepted.
// Safe for concurrent use.
type AuthReputation struct {
	client            redis.UniversalClient
	cfg               AuthReputationConfig
	trustedProxyDepth int
	mem               *authReputationMemory
}

// NewAuthReputation builds the guard. When client is nil (or a typed
// nil) a per-replica in-memory fallback is used so single-process and
// test deployments still get best-effort protection.
func NewAuthReputation(client redis.UniversalClient, cfg AuthReputationConfig, trustedProxyDepth int) *AuthReputation {
	if isNilRedisClient(client) {
		client = nil
	}
	if trustedProxyDepth < 0 {
		trustedProxyDepth = 0
	}
	a := &AuthReputation{
		client:            client,
		cfg:               cfg.withDefaults(),
		trustedProxyDepth: trustedProxyDepth,
	}
	if client == nil {
		a.mem = newAuthReputationMemory(a.cfg.Retention)
		go a.mem.runJanitor(5 * time.Minute)
	}
	return a
}

// recordFailureScript atomically increments the per-IP failure counter,
// refreshes its retention TTL, and — when the (new) count has reached
// the cooldown zone — stamps the next-allowed time. Doing the INCR and
// the penalty computation in one script avoids a read-modify-write race
// between concurrent failed attempts from the same IP.
//
// KEYS[1] = reputation hash for the IP
// ARGV    = nowMs, retentionSec, threshold, blockMs, nDelays, delayMs...
// Returns: {count, untilMs}
var recordFailureScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local retention = tonumber(ARGV[2])
local threshold = tonumber(ARGV[3])
local blockMs = tonumber(ARGV[4])
local nDelays = tonumber(ARGV[5])

local count = redis.call('HINCRBY', key, 'count', 1)
redis.call('EXPIRE', key, retention)

local penalty = 0
if count >= threshold then
  local idx = count - threshold        -- 0-based position past the threshold
  if idx < nDelays then
    penalty = tonumber(ARGV[6 + idx])
  else
    penalty = blockMs
  end
end

local untilMs = 0
if penalty > 0 then
  untilMs = now + penalty
  redis.call('HSET', key, 'until', untilMs)
  -- Make sure the hash lives at least until the cooldown elapses,
  -- even if that is longer than the retention window.
  local cooldownSec = math.ceil(penalty / 1000)
  if cooldownSec > retention then
    redis.call('EXPIRE', key, cooldownSec)
  end
end
return {count, untilMs}
`)

func reputationKey(ip string) string { return "authrep:" + ip }

// Penalty returns the cooldown the IP must respect before its next
// sign-in attempt is accepted. Zero means "go ahead". Fails open.
func (a *AuthReputation) Penalty(ctx context.Context, ip string, now time.Time) time.Duration {
	if ip == "" {
		return 0
	}
	if a.client == nil {
		return a.mem.penalty(ip, now)
	}
	raw, err := a.client.HGet(ctx, reputationKey(ip), "until").Result()
	if err == redis.Nil {
		return 0
	}
	if err != nil {
		logging.FromContext(ctx).Error("auth reputation penalty check failed, allowing attempt", "err", err)
		return 0
	}
	untilMs, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil {
		return 0
	}
	until := time.UnixMilli(untilMs)
	if wait := until.Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// RecordFailure registers a failed sign-in from ip and returns the
// cooldown now in effect for the IP (0 until the threshold is crossed).
// Fails open.
func (a *AuthReputation) RecordFailure(ctx context.Context, ip string, now time.Time) time.Duration {
	if ip == "" {
		return 0
	}
	if a.client == nil {
		return a.mem.recordFailure(ip, now, a.cfg)
	}
	argv := []any{
		now.UnixMilli(),
		int64(a.cfg.Retention.Seconds()),
		a.cfg.FailureThreshold,
		a.cfg.BlockDuration.Milliseconds(),
		len(a.cfg.Delays),
	}
	for _, d := range a.cfg.Delays {
		argv = append(argv, d.Milliseconds())
	}
	res, err := recordFailureScript.Run(ctx, a.client, []string{reputationKey(ip)}, argv...).Slice()
	if err != nil {
		logging.FromContext(ctx).Error("auth reputation record-failure failed", "err", err)
		return 0
	}
	if len(res) < 2 {
		return 0
	}
	untilMs, _ := res[1].(int64)
	if untilMs <= 0 {
		return 0
	}
	if wait := time.UnixMilli(untilMs).Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// Reset clears an IP's reputation after a successful sign-in. Fails
// open (a failed reset only means the IP keeps a stale counter that
// will expire on its own).
func (a *AuthReputation) Reset(ctx context.Context, ip string) {
	if ip == "" {
		return
	}
	if a.client == nil {
		a.mem.reset(ip)
		return
	}
	if err := a.client.Del(ctx, reputationKey(ip)).Err(); err != nil {
		logging.FromContext(ctx).Warn("auth reputation reset failed", "err", err)
	}
}

// clientIP resolves the request's client IP honouring the configured
// trusted-proxy depth. Empty string when undeterminable.
func (a *AuthReputation) clientIP(r *http.Request) string {
	if ip := ClientIPFromRequest(r, a.trustedProxyDepth); ip != nil {
		return ip.String()
	}
	return ""
}

// AuthReputationGuard wraps an authentication endpoint (e.g. /login)
// with the brute-force guard. It rejects attempts from an IP currently
// in cooldown (429 + Retry-After), and after dispatch records a failure
// for a 401 response or resets the counter for a 2xx response.
//
// A nil guard returns a pass-through middleware so wiring code can stay
// branch-free when the feature is disabled.
func AuthReputationGuard(rep *AuthReputation) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if rep == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := rep.clientIP(r)
			now := time.Now()
			if wait := rep.Penalty(r.Context(), ip, now); wait > 0 {
				writeAuthThrottled(w, wait)
				return
			}

			// Buffer the auth response so the guard can inject a
			// Retry-After header on the threshold-crossing 401 BEFORE
			// the status line is flushed. Setting a header after the
			// handler's WriteHeader would be a silent no-op on a real
			// ResponseWriter. Auth responses are tiny JSON bodies, so
			// buffering them costs nothing and /login never streams.
			rec := &bufferingWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			switch {
			case rec.status == http.StatusUnauthorized:
				if wait := rep.RecordFailure(r.Context(), ip, time.Now()); wait > 0 {
					// Surface the now-active cooldown so a client
					// honouring Retry-After backs off immediately
					// rather than on its next (rejected) attempt.
					retryAfterHeader(w, wait)
				}
			case rec.status >= 200 && rec.status < 300:
				rep.Reset(r.Context(), ip)
			}
			rec.flush()
		})
	}
}

// writeAuthThrottled emits the 429 for an IP in cooldown, with a
// Retry-After the client can honour.
func writeAuthThrottled(w http.ResponseWriter, wait time.Duration) {
	retryAfterHeader(w, wait)
	RespondError(w, http.StatusTooManyRequests, ErrCodeAuthThrottled, "too many failed sign-in attempts; please wait before retrying")
}

func retryAfterHeader(w http.ResponseWriter, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

// bufferingWriter captures the downstream handler's status and body so
// the guard can react to the auth outcome (401 vs 2xx) AND mutate
// response headers (e.g. inject Retry-After on the threshold 401)
// before anything is flushed to the client. flush() must be called
// exactly once after the handler returns. Used only for the small,
// non-streaming auth responses, so full buffering is cheap and safe.
type bufferingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         bytes.Buffer
}

func (w *bufferingWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status = status
		w.wroteHeader = true
	}
}

func (w *bufferingWriter) Write(b []byte) (int, error) {
	// A handler that writes a body without an explicit WriteHeader has
	// implicitly sent 200 (the struct's default status).
	w.wroteHeader = true
	return w.buf.Write(b)
}

// flush writes the buffered status line and body to the underlying
// ResponseWriter. Any headers set on the underlying writer (by the
// handler or the guard) go out with the status line.
func (w *bufferingWriter) flush() {
	w.ResponseWriter.WriteHeader(w.status)
	if w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
	}
}

// --- in-memory fallback ---

type authReputationEntry struct {
	count int
	until time.Time
	seen  time.Time
}

type authReputationMemory struct {
	retention time.Duration
	mu        sync.Mutex
	entries   map[string]*authReputationEntry
}

func newAuthReputationMemory(retention time.Duration) *authReputationMemory {
	if retention <= 0 {
		retention = DefaultAuthReputationRetention
	}
	return &authReputationMemory{retention: retention, entries: make(map[string]*authReputationEntry)}
}

func (m *authReputationMemory) penalty(ip string, now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[ip]
	if e == nil {
		return 0
	}
	if wait := e.until.Sub(now); wait > 0 {
		return wait
	}
	return 0
}

func (m *authReputationMemory) recordFailure(ip string, now time.Time, cfg AuthReputationConfig) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[ip]
	if e == nil || now.Sub(e.seen) > cfg.Retention {
		e = &authReputationEntry{}
		m.entries[ip] = e
	}
	e.count++
	e.seen = now

	var penalty time.Duration
	if e.count >= cfg.FailureThreshold {
		idx := e.count - cfg.FailureThreshold
		if idx < len(cfg.Delays) {
			penalty = cfg.Delays[idx]
		} else {
			penalty = cfg.BlockDuration
		}
	}
	if penalty > 0 {
		e.until = now.Add(penalty)
		return penalty
	}
	return 0
}

func (m *authReputationMemory) reset(ip string) {
	m.mu.Lock()
	delete(m.entries, ip)
	m.mu.Unlock()
}

func (m *authReputationMemory) runJanitor(window time.Duration) {
	t := time.NewTicker(window)
	defer t.Stop()
	for now := range t.C {
		m.mu.Lock()
		for ip, e := range m.entries {
			// Drop entries that are both past their cooldown and
			// idle beyond the retention window.
			if now.After(e.until) && now.Sub(e.seen) > m.retention {
				delete(m.entries, ip)
			}
		}
		m.mu.Unlock()
	}
}
