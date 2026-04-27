// Package session provides Redis-backed session storage for the
// zk-drive server.
//
// Session state used to live entirely inside JWTs, which made
// revocation impossible without rotating the signing secret. The
// store recorded here lets the server invalidate individual sessions
// or every session for a given user — both required by the admin
// "force sign-out" flow and the password-rotation path.
//
// Keys are namespaced by workspace per ARCHITECTURE.md §9 ("Redis key
// prefixing — session and cache keys are namespaced by workspace_id
// so a Redis misread cannot leak across tenants"). The full layout
// is:
//
//	ws:{workspaceID}:session:{sessionID}        # HASH (user_id, workspace_id)
//	ws:{workspaceID}:user_sessions:{userID}     # SET of sessionIDs
//
// The user-sessions SET is a secondary index used by RevokeAllForUser
// so we can wipe every active session for a given identity without
// scanning the keyspace.
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ErrSessionNotFound is returned by Get when the session ID is
// unknown or has been revoked. Callers translate this to a 401 so the
// client knows to re-authenticate.
var ErrSessionNotFound = errors.New("session not found")

// RedisSessionStore persists session metadata in Redis so revocation
// works across multiple replicas of the API server.
type RedisSessionStore struct {
	client redis.UniversalClient
}

// NewRedisSessionStore wraps an existing Redis client. Callers are
// responsible for the client's lifecycle (Close on shutdown).
func NewRedisSessionStore(client redis.UniversalClient) *RedisSessionStore {
	return &RedisSessionStore{client: client}
}

// sessionKey returns the workspace-scoped HASH key for a session.
func sessionKey(workspaceID uuid.UUID, sessionID string) string {
	return fmt.Sprintf("ws:%s:session:%s", workspaceID.String(), sessionID)
}

// userSessionsKey returns the workspace-scoped SET key listing every
// active session for a user.
func userSessionsKey(workspaceID, userID uuid.UUID) string {
	return fmt.Sprintf("ws:%s:user_sessions:%s", workspaceID.String(), userID.String())
}

// Set stores a session hash and registers it in the user's secondary
// index. The TTL is applied to both keys so an abandoned session
// expires from the index along with the hash and we never accumulate
// dangling references.
func (s *RedisSessionStore) Set(ctx context.Context, sessionID string, userID, workspaceID uuid.UUID, ttl time.Duration) error {
	if sessionID == "" {
		return errors.New("session id required")
	}
	skey := sessionKey(workspaceID, sessionID)
	ukey := userSessionsKey(workspaceID, userID)

	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, skey, map[string]any{
		"user_id":      userID.String(),
		"workspace_id": workspaceID.String(),
	})
	pipe.Expire(ctx, skey, ttl)
	pipe.SAdd(ctx, ukey, sessionID)
	pipe.Expire(ctx, ukey, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis session set: %w", err)
	}
	return nil
}

// Get reads a session hash and returns the bound user / workspace.
// Returns ErrSessionNotFound when the session has been revoked or
// expired so callers can treat both cases uniformly.
//
// The workspace ID must be supplied because session IDs alone are not
// workspace-scoped on the wire — the caller already knows which
// tenant context the request is operating in (set by the auth
// middleware) and we use that to look up the right hash.
func (s *RedisSessionStore) Get(ctx context.Context, workspaceID uuid.UUID, sessionID string) (userID, ws uuid.UUID, err error) {
	if sessionID == "" {
		return uuid.Nil, uuid.Nil, ErrSessionNotFound
	}
	values, err := s.client.HGetAll(ctx, sessionKey(workspaceID, sessionID)).Result()
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("redis session get: %w", err)
	}
	if len(values) == 0 {
		return uuid.Nil, uuid.Nil, ErrSessionNotFound
	}
	uidStr, ok := values["user_id"]
	if !ok {
		return uuid.Nil, uuid.Nil, ErrSessionNotFound
	}
	wsStr, ok := values["workspace_id"]
	if !ok {
		return uuid.Nil, uuid.Nil, ErrSessionNotFound
	}
	uid, err := uuid.Parse(uidStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("parse user_id: %w", err)
	}
	wid, err := uuid.Parse(wsStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("parse workspace_id: %w", err)
	}
	return uid, wid, nil
}

// Revoke deletes a single session. The hash is read first so we can
// also remove the entry from the user-sessions secondary index;
// otherwise RevokeAllForUser would later try (and fail) to delete a
// hash that no longer exists. Both deletes happen in one pipeline so
// we don't race a parallel Set.
func (s *RedisSessionStore) Revoke(ctx context.Context, workspaceID uuid.UUID, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	skey := sessionKey(workspaceID, sessionID)
	uidStr, err := s.client.HGet(ctx, skey, "user_id").Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("redis session revoke lookup: %w", err)
	}

	pipe := s.client.TxPipeline()
	pipe.Del(ctx, skey)
	if uidStr != "" {
		if uid, perr := uuid.Parse(uidStr); perr == nil {
			pipe.SRem(ctx, userSessionsKey(workspaceID, uid), sessionID)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis session revoke: %w", err)
	}
	return nil
}

// RevokeAllForUser deletes every active session for a user by
// iterating the secondary index. SMembers is bounded by the user's
// active session count (small in practice — single digits) so the
// pipeline stays cheap even when a user has logged in from several
// devices.
func (s *RedisSessionStore) RevokeAllForUser(ctx context.Context, workspaceID, userID uuid.UUID) error {
	ukey := userSessionsKey(workspaceID, userID)
	ids, err := s.client.SMembers(ctx, ukey).Result()
	if err != nil {
		return fmt.Errorf("redis session list: %w", err)
	}
	if len(ids) == 0 {
		// Even an empty set leaves the key behind; clean it up so
		// future calls don't pay the SMembers round-trip.
		if err := s.client.Del(ctx, ukey).Err(); err != nil {
			return fmt.Errorf("redis session index del: %w", err)
		}
		return nil
	}

	pipe := s.client.TxPipeline()
	for _, id := range ids {
		pipe.Del(ctx, sessionKey(workspaceID, id))
	}
	pipe.Del(ctx, ukey)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis session revoke-all: %w", err)
	}
	return nil
}
