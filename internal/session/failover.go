package session

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// FailoverStore makes the Redis-backed session store seamlessly
// degrade to a process-local MemoryStore when Redis is unreachable,
// and recover automatically when it returns (WS8 8.4 server
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
// Trade-off (documented and intentional): while degraded, revocation
// state is per-replica rather than shared. A logout/force-sign-out is
// honoured on the replica that served it, but a sibling replica won't
// see it until Redis recovers. For the single-node SME profile this
// product targets there are no siblings, so behaviour is identical to
// healthy Redis; for multi-replica deployments this is the correct
// availability-over-consistency choice for a transient outage, and the
// warning log makes the degraded state visible.
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
			if err == nil && f.healthy.CompareAndSwap(false, true) {
				f.logger.Info("redis session store recovered, resuming shared session/revocation backend")
			}
		}
	}
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

// Compile-time assertion that FailoverStore satisfies Store.
var _ Store = (*FailoverStore)(nil)
