package session

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store is the full set of session operations the server depends on,
// satisfied by *RedisSessionStore (the shared, cross-replica backend)
// and by *MemoryStore (the per-replica fallback). Pulling the surface
// behind an interface lets FailoverStore route each call to whichever
// backend is currently healthy without the callers (auth handler,
// platform service, auth middleware) knowing which one served them.
type Store interface {
	Set(ctx context.Context, sessionID string, userID, workspaceID uuid.UUID, ttl time.Duration) error
	Get(ctx context.Context, workspaceID uuid.UUID, sessionID string) (userID, ws uuid.UUID, err error)
	Revoke(ctx context.Context, workspaceID uuid.UUID, sessionID string) error
	RevokeAllForUser(ctx context.Context, workspaceID, userID uuid.UUID) error
	RevokeUser(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time, ttl time.Duration) error
	IsRevoked(ctx context.Context, workspaceID, userID uuid.UUID, issuedAt time.Time) (bool, error)
}

// Compile-time assertions that both backends satisfy Store.
var (
	_ Store = (*RedisSessionStore)(nil)
	_ Store = (*MemoryStore)(nil)
)

// MemoryStore is a process-local implementation of Store used as the
// seamless fallback when Redis is unreachable (WS8 8.4 server
// self-healing) and as the primary store for single-replica
// deployments that run without Redis at all.
//
// Semantics mirror RedisSessionStore exactly, including the
// workspace-scoped key layout, so a failover mid-request behaves
// identically from the caller's perspective. The one property it
// cannot provide is cross-replica sharing: a revocation recorded on
// one replica's MemoryStore is invisible to another replica. That is
// the documented and accepted degradation of the in-memory fallback —
// it is exactly right for the single-node SME profile this product
// targets, and for multi-replica deployments it is the correct
// availability-over-consistency trade for a transient Redis outage.
//
// All maps are guarded by a single mutex. Session volumes per replica
// are modest (active sessions, not historical), so a coarse lock is
// simpler and entirely adequate; the hot path (IsRevoked on every
// request) is a single map read under the lock.
type MemoryStore struct {
	mu sync.Mutex

	// sessions maps sessionKey(ws,sid) -> bound identity + expiry.
	sessions map[string]memSession
	// userIndex maps userSessionsKey(ws,uid) -> set of session IDs
	// with its own expiry, mirroring the Redis secondary index used
	// by RevokeAllForUser.
	userIndex map[string]*memUserIndex
	// revoked maps userRevokedKey(ws,uid) -> revocation cutoff + expiry.
	revoked map[string]memRevocation

	now func() time.Time
}

type memSession struct {
	userID      uuid.UUID
	workspaceID uuid.UUID
	expiresAt   time.Time
}

type memUserIndex struct {
	sessionIDs map[string]struct{}
	expiresAt  time.Time
}

type memRevocation struct {
	workspaceID uuid.UUID
	userID      uuid.UUID
	cutoff      int64 // unix seconds
	expiresAt   time.Time
}

// NewMemoryStore returns an empty in-memory session store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions:  make(map[string]memSession),
		userIndex: make(map[string]*memUserIndex),
		revoked:   make(map[string]memRevocation),
		now:       time.Now,
	}
}

// Set stores a session and registers it in the user's secondary index.
func (m *MemoryStore) Set(_ context.Context, sessionID string, userID, workspaceID uuid.UUID, ttl time.Duration) error {
	if sessionID == "" {
		return errSessionIDRequired
	}
	now := m.now()
	exp := now.Add(ttl)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionKey(workspaceID, sessionID)] = memSession{
		userID:      userID,
		workspaceID: workspaceID,
		expiresAt:   exp,
	}
	ukey := userSessionsKey(workspaceID, userID)
	idx := m.userIndex[ukey]
	if idx == nil {
		idx = &memUserIndex{sessionIDs: make(map[string]struct{})}
		m.userIndex[ukey] = idx
	}
	idx.sessionIDs[sessionID] = struct{}{}
	// Index TTL is max(current, ttl): the index must outlive its
	// longest-lived member so RevokeAllForUser can still find it.
	if exp.After(idx.expiresAt) {
		idx.expiresAt = exp
	}
	return nil
}

// Get returns the identity bound to a session, or ErrSessionNotFound
// if it is unknown, revoked, or expired.
func (m *MemoryStore) Get(_ context.Context, workspaceID uuid.UUID, sessionID string) (uuid.UUID, uuid.UUID, error) {
	if sessionID == "" {
		return uuid.Nil, uuid.Nil, ErrSessionNotFound
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionKey(workspaceID, sessionID)
	s, ok := m.sessions[key]
	if !ok || !s.expiresAt.After(now) {
		if ok {
			delete(m.sessions, key) // lazily reap the expired entry
		}
		return uuid.Nil, uuid.Nil, ErrSessionNotFound
	}
	return s.userID, s.workspaceID, nil
}

// Revoke deletes a single session and removes it from the user index.
func (m *MemoryStore) Revoke(_ context.Context, workspaceID uuid.UUID, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionKey(workspaceID, sessionID)
	if s, ok := m.sessions[key]; ok {
		delete(m.sessions, key)
		ukey := userSessionsKey(workspaceID, s.userID)
		if idx := m.userIndex[ukey]; idx != nil {
			delete(idx.sessionIDs, sessionID)
			if len(idx.sessionIDs) == 0 {
				delete(m.userIndex, ukey)
			}
		}
	}
	return nil
}

// RevokeAllForUser deletes every active session for a user.
func (m *MemoryStore) RevokeAllForUser(_ context.Context, workspaceID, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ukey := userSessionsKey(workspaceID, userID)
	idx := m.userIndex[ukey]
	if idx != nil {
		for sid := range idx.sessionIDs {
			delete(m.sessions, sessionKey(workspaceID, sid))
		}
		delete(m.userIndex, ukey)
	}
	return nil
}

// RevokeUser records a per-user revocation cutoff: any token issued at
// or before `at` is treated as revoked. The stored cutoff only ever
// moves forward (max-update), matching the Redis Lua script, so an
// out-of-order revocation cannot re-validate already-rejected tokens.
func (m *MemoryStore) RevokeUser(_ context.Context, workspaceID, userID uuid.UUID, at time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	cutoff := at.UTC().Unix()
	exp := m.now().Add(ttl)
	key := userRevokedKey(workspaceID, userID)
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.revoked[key]
	if !ok || cutoff > cur.cutoff {
		m.revoked[key] = memRevocation{
			workspaceID: workspaceID,
			userID:      userID,
			cutoff:      cutoff,
			expiresAt:   exp,
		}
	}
	return nil
}

// snapshotRevocations returns the live (non-expired) per-user
// revocation cutoffs held in memory. It exists so a FailoverStore can
// replay revocations recorded while degraded back into Redis on
// recovery (flush-on-recovery), closing the window where a
// force-sign-out issued during an outage would otherwise be forgotten
// once reads resume against Redis. Expired entries are skipped (and
// lazily reaped) so a flush never resurrects a cutoff past its TTL.
func (m *MemoryStore) snapshotRevocations() []revocationRecord {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]revocationRecord, 0, len(m.revoked))
	for k, r := range m.revoked {
		if !r.expiresAt.After(now) {
			delete(m.revoked, k)
			continue
		}
		out = append(out, revocationRecord{
			workspaceID: r.workspaceID,
			userID:      r.userID,
			cutoff:      time.Unix(r.cutoff, 0).UTC(),
			expiresAt:   r.expiresAt,
		})
	}
	return out
}

// IsRevoked reports whether the per-user cutoff is at or after
// issuedAt. A missing (or expired) cutoff means "never revoked".
func (m *MemoryStore) IsRevoked(_ context.Context, workspaceID, userID uuid.UUID, issuedAt time.Time) (bool, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	key := userRevokedKey(workspaceID, userID)
	r, ok := m.revoked[key]
	if !ok || !r.expiresAt.After(now) {
		if ok {
			delete(m.revoked, key)
		}
		return false, nil
	}
	return issuedAt.UTC().Unix() <= r.cutoff, nil
}

// reap evicts expired sessions, user-index entries, and revocation
// cutoffs. Called by the janitor; safe to run concurrently with the
// store's own methods.
func (m *MemoryStore) reap() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if !s.expiresAt.After(now) {
			delete(m.sessions, k)
		}
	}
	for k, idx := range m.userIndex {
		if !idx.expiresAt.After(now) {
			delete(m.userIndex, k)
		}
	}
	for k, r := range m.revoked {
		if !r.expiresAt.After(now) {
			delete(m.revoked, k)
		}
	}
}

// RunJanitor periodically reaps expired entries until ctx is
// cancelled. Intended to be launched in its own goroutine. Without it
// the maps would retain expired-but-unread sessions indefinitely;
// lazy per-read expiry handles correctness, the janitor handles the
// memory footprint of keys that are never read again.
func (m *MemoryStore) RunJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reap()
		}
	}
}
