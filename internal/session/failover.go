package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// FailoverStore makes the Redis-backed session store seamlessly
// degrade to a process-local MemoryStore when Redis is unreachable,
// and recover automatically when it returns (server
// self-healing).
//
// Before this, a Redis outage that began *after* startup turned every
// authenticated request into a 401 (IsRevoked fails closed) and broke
// login/logout — the operator had to notice and intervene. FailoverStore
// removes that operator burden: the moment a Redis command fails with a
// connectivity error it routes the call (and subsequent calls) to the
// in-memory store, logs a single warning, and a background pinger flips
// back to Redis as soon as it answers again.
//
// Trade-off (documented and intentional): while degraded, newly
// created sessions live only in the per-replica in-memory store. A
// session created mid-outage is therefore not shared with siblings and
// is dropped on recovery (the user re-authenticates once) — the
// accepted availability-over-consistency cost of serving through an
// outage instead of 401-ing every request. For the same reason the
// device-aware ValidateSession gate degrades OPEN for a session the
// fallback has never seen (one created before the outage, living only
// in the unreachable Redis): it is admitted rather than 401'd, exactly
// as the IsRevoked hot path degrades open on the same outage. A
// pre-outage per-session revocation is invisible for the bounded
// outage window and re-enforced automatically the instant Redis
// recovers; a session the fallback DOES know (created mid-outage) stays
// fully device-bound. See FailoverStore.ValidateSession.
//
// Revocations are the security-sensitive exception and are NOT simply
// dropped. A force-sign-out / logout recorded while degraded is
// replayed into Redis on recovery (see flushRevocations, invoked by
// RunHealthLoop before primary reads resume). Without that, a token
// deliberately killed during an outage would silently come back to
// life the moment IsRevoked started consulting Redis again — a
// privilege the in-memory cutoff was specifically created to deny.
// Revocation cutoffs are monotonic (max-update) and replay is
// idempotent, so flushing can only ever tighten access, never loosen
// it, and is safe under concurrent multi-replica recovery.
type FailoverStore struct {
	primary  Store // Redis-backed
	fallback Store // in-memory
	ping     func(ctx context.Context) error
	logger   *slog.Logger

	// healthy is the authoritative routing flag. It is flipped to
	// false the instant a primary call returns a connectivity error
	// and back to true by the background health loop once Redis
	// answers a PING. Stored as an atomic so the hot read path
	// (IsRevoked on every request) is lock-free.
	healthy atomic.Bool
	// pingTimeout bounds a single recovery PING.
	pingTimeout time.Duration
}

// NewFailoverStore wires a FailoverStore over a healthy Redis client.
// primary is the Redis store, fallback the in-memory store, and ping
// is the health probe used to detect recovery (typically
// client.Ping). The store starts healthy (the caller only constructs
// it after a successful startup ping) and must be driven with
// RunHealthLoop to recover after an outage.
func NewFailoverStore(primary, fallback Store, ping func(ctx context.Context) error, logger *slog.Logger) *FailoverStore {
	if logger == nil {
		logger = slog.Default()
	}
	f := &FailoverStore{
		primary:     primary,
		fallback:    fallback,
		ping:        ping,
		logger:      logger,
		pingTimeout: 2 * time.Second,
	}
	f.healthy.Store(true)
	return f
}

// isUnavailable reports whether err indicates Redis is unreachable (as
// opposed to a logical result like redis.Nil or a context cancellation
// driven by the caller). Connectivity failures surface as net errors,
// a closed client/pool, or pool-timeout — anything else (a Lua error,
// a WRONGTYPE) is a real error the caller should see, not a reason to
// fail over.
func isUnavailable(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	// A caller-driven cancellation/deadline is not a Redis outage.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// A bare io.EOF (or io.ErrUnexpectedEOF) surfaces when Redis closes
	// the connection mid-command — a graceful server shutdown or a TLS
	// layer teardown — without wrapping it in a net.OpError. Treat it as
	// an outage so the very first request fails over immediately instead
	// of failing closed and waiting for the next request to see a
	// connection-refused net error.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, redis.ErrClosed) ||
		errors.Is(err, redis.ErrPoolExhausted) ||
		errors.Is(err, redis.ErrPoolTimeout) {
		return true
	}
	return false
}

// markDown flips the routing flag to degraded and logs the transition
// exactly once (subsequent failures while already-down are silent, so
// a prolonged outage does not flood the log).
func (f *FailoverStore) markDown(op string, err error) {
	if f.healthy.CompareAndSwap(true, false) {
		f.logger.Warn("redis session store unreachable, failing over to in-memory store (per-replica revocation until redis recovers)",
			"op", op, "err", err)
	}
}

// Healthy reports whether the store is currently routing to Redis.
// Exposed for the admin health dashboard / tests.
func (f *FailoverStore) Healthy() bool { return f.healthy.Load() }

// RunHealthLoop pings Redis on a fixed cadence while the store is
// degraded and flips back to the primary on the first successful
// reply, logging the recovery. It is a no-op tick while healthy (a
// cheap atomic read), so it is safe to run at a brisk cadence. Launch
// in its own goroutine; returns when ctx is cancelled.
func (f *FailoverStore) RunHealthLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if f.healthy.Load() || f.ping == nil {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, f.pingTimeout)
			err := f.ping(pctx)
			cancel()
			if err != nil {
				continue
			}
			// Replay revocations recorded while degraded into Redis
			// BEFORE flipping reads back to it, so there is no instant
			// where IsRevoked consults a Redis that has forgotten a
			// force-sign-out issued during the outage. If the flush
			// itself hits a Redis blip we stay degraded and retry on
			// the next tick rather than resuming with a gap.
			fctx, fcancel := context.WithTimeout(ctx, f.pingTimeout)
			ferr := f.flushRevocations(fctx)
			fcancel()
			if ferr != nil {
				f.logger.Warn("redis answered but revocation flush failed, staying degraded", "err", ferr)
				continue
			}
			if f.healthy.CompareAndSwap(false, true) {
				// A revocation may have landed in the fallback in the
				// narrow window between the flush and the flip; replay
				// once more (idempotent) so it is not stranded now that
				// reads route to Redis.
				f2ctx, f2cancel := context.WithTimeout(ctx, f.pingTimeout)
				if err := f.flushRevocations(f2ctx); err != nil {
					f.logger.Warn("post-recovery revocation re-flush failed (cutoffs expire on their own TTL)", "err", err)
				}
				f2cancel()
				f.logger.Info("redis session store recovered, resuming shared session/revocation backend")
			}
		}
	}
}

// revocationRecord is a single per-user revocation cutoff exported from
// the in-memory fallback for replay into Redis on recovery.
type revocationRecord struct {
	workspaceID uuid.UUID
	userID      uuid.UUID
	cutoff      time.Time
	expiresAt   time.Time
}

// revocationSnapshotter is implemented by the in-memory store so the
// FailoverStore can read back the cutoffs it recorded while degraded
// without widening the Store interface (the Redis store has no need to
// export its state). The fallback is always a *MemoryStore in
// production; the type assertion in flushRevocations degrades to a
// no-op for any fallback that does not implement it.
type revocationSnapshotter interface {
	snapshotRevocations() []revocationRecord
}

// flushRevocations replays every live revocation cutoff held in the
// fallback into the primary (Redis). It is called on recovery so a
// force-sign-out issued during an outage survives the switch back to
// Redis. RevokeUser is monotonic (max-update) and idempotent, so
// replaying an already-present cutoff is a no-op and concurrent
// replays from multiple recovering replicas converge safely. A
// connectivity error is returned so the caller keeps the store
// degraded and retries; a non-connectivity error on a single entry is
// logged and skipped so one bad row cannot block recovery.
func (f *FailoverStore) flushRevocations(ctx context.Context) error {
	snap, ok := f.fallback.(revocationSnapshotter)
	if !ok {
		return nil
	}
	now := time.Now()
	for _, r := range snap.snapshotRevocations() {
		ttl := r.expiresAt.Sub(now)
		if ttl <= 0 {
			continue // already expired; nothing to preserve
		}
		if err := f.primary.RevokeUser(ctx, r.workspaceID, r.userID, r.cutoff, ttl); err != nil {
			if isUnavailable(err) {
				return err
			}
			f.logger.Warn("replaying degraded-window revocation into redis failed",
				"workspace_id", r.workspaceID, "user_id", r.userID, "err", err)
		}
	}
	return nil
}

func (f *FailoverStore) Set(ctx context.Context, sessionID string, userID, workspaceID uuid.UUID, ttl time.Duration) error {
	if f.healthy.Load() {
		err := f.primary.Set(ctx, sessionID, userID, workspaceID, ttl)
		if !isUnavailable(err) {
			return err
		}
		f.markDown("Set", err)
	}
	return f.fallback.Set(ctx, sessionID, userID, workspaceID, ttl)
}

func (f *FailoverStore) Get(ctx context.Context, workspaceID uuid.UUID, sessionID string) (uuid.UUID, uuid.UUID, error) {
	if f.healthy.Load() {
		uid, ws, err := f.primary.Get(ctx, workspaceID, sessionID)
		if !isUnavailable(err) {
			return uid, ws, err
		}
		f.markDown("Get", err)
	}
	return f.fallback.Get(ctx, workspaceID, sessionID)
}

func (f *FailoverStore) Revoke(ctx context.Context, workspaceID uuid.UUID, sessionID string) error {
	if f.healthy.Load() {
		err := f.primary.Revoke(ctx, workspaceID, sessionID)
		if !isUnavailable(err) {
			return err
		}
		f.markDown("Revoke", err)
	}
	return f.fallback.Revoke(ctx, workspaceID, sessionID)
}

func (f *FailoverStore) RevokeAllForUser(ctx context.Context, workspaceID, userID uuid.UUID) error {
	if f.healthy.Load() {
		err := f.primary.RevokeAllForUser(ctx, workspaceID, userID)
		if !isUnavailable(err) {
			return err
		}
		f.markDown("RevokeAllForUser", err)
	}
	return f.fallback.RevokeAllForUser(ctx, workspaceID, userID)
}

func (f *FailoverStore) RevokeUser(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time, ttl time.Duration) error {
	if f.healthy.Load() {
		err := f.primary.RevokeUser(ctx, workspaceID, userID, at, ttl)
		if !isUnavailable(err) {
			return err
		}
		f.markDown("RevokeUser", err)
	}
	return f.fallback.RevokeUser(ctx, workspaceID, userID, at, ttl)
}

func (f *FailoverStore) IsRevoked(ctx context.Context, workspaceID, userID uuid.UUID, issuedAt time.Time) (bool, error) {
	if f.healthy.Load() {
		revoked, err := f.primary.IsRevoked(ctx, workspaceID, userID, issuedAt)
		if !isUnavailable(err) {
			return revoked, err
		}
		f.markDown("IsRevoked", err)
	}
	return f.fallback.IsRevoked(ctx, workspaceID, userID, issuedAt)
}

func (f *FailoverStore) Create(ctx context.Context, rec SessionRecord, ttl time.Duration) error {
	if f.healthy.Load() {
		err := f.primary.Create(ctx, rec, ttl)
		if !isUnavailable(err) {
			return err
		}
		f.markDown("Create", err)
	}
	return f.fallback.Create(ctx, rec, ttl)
}

func (f *FailoverStore) GetRecord(ctx context.Context, workspaceID uuid.UUID, sessionID string) (SessionRecord, error) {
	if f.healthy.Load() {
		rec, err := f.primary.GetRecord(ctx, workspaceID, sessionID)
		if !isUnavailable(err) {
			return rec, err
		}
		f.markDown("GetRecord", err)
	}
	return f.fallback.GetRecord(ctx, workspaceID, sessionID)
}

func (f *FailoverStore) ListForUser(ctx context.Context, workspaceID, userID uuid.UUID) ([]SessionRecord, error) {
	if f.healthy.Load() {
		recs, err := f.primary.ListForUser(ctx, workspaceID, userID)
		if !isUnavailable(err) {
			return recs, err
		}
		f.markDown("ListForUser", err)
	}
	return f.fallback.ListForUser(ctx, workspaceID, userID)
}

func (f *FailoverStore) RevokeForUser(ctx context.Context, workspaceID, userID uuid.UUID, sessionID string) (bool, error) {
	if f.healthy.Load() {
		ok, err := f.primary.RevokeForUser(ctx, workspaceID, userID, sessionID)
		if !isUnavailable(err) {
			return ok, err
		}
		f.markDown("RevokeForUser", err)
	}
	return f.fallback.RevokeForUser(ctx, workspaceID, userID, sessionID)
}

func (f *FailoverStore) ValidateSession(ctx context.Context, workspaceID uuid.UUID, sessionID, userAgent, clientIP string) error {
	if f.healthy.Load() {
		err := f.primary.ValidateSession(ctx, workspaceID, sessionID, userAgent, clientIP)
		// A device anomaly / not-found from a reachable Redis is a real
		// auth decision, not a connectivity failure, so only an
		// unavailable error fails over.
		if !isUnavailable(err) {
			return err
		}
		f.markDown("ValidateSession", err)
	}
	// Degraded: the per-replica in-memory fallback only knows sessions
	// created on this replica during the outage. A session that predates
	// the outage lives solely in the now-unreachable Redis, so the
	// fallback reports ErrSessionNotFound for it. Hard-401ing every such
	// request would turn a transient Redis blip into a fleet-wide forced
	// re-login — the exact operator burden FailoverStore exists to remove
	// — and would be inconsistent with the IsRevoked hot path,
	// which degrades OPEN on the same outage (an empty fallback reports
	// "not revoked"). So a session unknown to the fallback is admitted
	// while degraded: the JWT signature/expiry and the per-user
	// revocation cutoff still gate the request, the window is bounded by
	// the health loop, and a pre-outage per-session revocation is
	// re-enforced automatically the instant Redis recovers (its hash is
	// still absent there). A session the fallback DOES know (created this
	// outage) is still fully validated below, including the device-
	// anomaly check, so an outage-created session stays device-bound.
	err := f.fallback.ValidateSession(ctx, workspaceID, sessionID, userAgent, clientIP)
	if errors.Is(err, ErrSessionNotFound) {
		return nil
	}
	return err
}

// Compile-time assertion that FailoverStore satisfies Store.
var _ Store = (*FailoverStore)(nil)
