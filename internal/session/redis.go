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
//	ws:{workspaceID}:user_revoked:{userID}      # STRING: unix-seconds cutoff
//
// The user-sessions SET is a secondary index used by RevokeAllForUser
// so we can wipe every active session for a given identity without
// scanning the keyspace.
//
// The user-revoked STRING records a per-user "tokens with iat <= this
// timestamp are no longer valid" cutoff. It is the mechanism that
// makes stateless JWT revocation actually work: AuthMiddleware
// consults it on every request via the SessionChecker interface, so
// a Logout call propagates to every replica within one Redis round
// trip without rotating the JWT signing secret. The cutoff TTLs at
// the JWT TTL so the key gracefully self-cleans after no token it
// could revoke remains valid.
package session

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ErrSessionNotFound is returned by Get when the session ID is
// unknown or has been revoked. Callers translate this to a 401 so the
// client knows to re-authenticate.
var ErrSessionNotFound = errors.New("session not found")

// errSessionIDRequired is returned by Set when called with an empty
// session ID. Shared by the Redis and memory stores so both backends
// reject the same invalid input identically.
var errSessionIDRequired = errors.New("session id required")

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

// userRevokedKey returns the workspace-scoped STRING key recording
// the per-user revocation cutoff (unix-seconds). Tokens with `iat`
// less than or equal to this value are considered revoked.
func userRevokedKey(workspaceID, userID uuid.UUID) string {
	return fmt.Sprintf("ws:%s:user_revoked:%s", workspaceID.String(), userID.String())
}

// revokeUserScript implements an atomic max-update for the per-user
// revocation cutoff. Without it, two concurrent RevokeUser calls
// could race such that a stale (earlier) timestamp lands second and
// moves the cutoff *backwards* — re-validating tokens an earlier
// revocation intended to reject.
//
// The script:
//
//  1. GETs the current cutoff (nil if no previous revocation).
//  2. Iff the new timestamp is greater than what's stored (or no
//     value is stored yet), SETs the new timestamp with an EXPIRE
//     equal to the requested TTL.
//  3. Always refreshes the TTL to the new value when the new
//     timestamp wins, so a recent revocation extends the key's
//     lifetime accordingly.
//
// We pass the cutoff and TTL as ARGV (numeric) rather than as
// separate Lua locals so the script stays uniform across go-redis
// versions and so the EX refresh is part of the same atomic block
// as the value comparison.
//
// The script also self-heals around a corrupted stored value: if
// GET returns a non-numeric string (manual Redis surgery, memory
// corruption, an older value format), `tonumber` evaluates to nil
// and the `not current_num` branch overwrites the bad value with
// the well-formed new cutoff. Without that fallback, every
// subsequent RevokeUser call would raise a Lua arithmetic error
// (`nil < new`) and the key would stay corrupted until manual
// intervention.
var revokeUserScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
local current_num = current and tonumber(current)
local new = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
if not current_num or current_num < new then
	redis.call('SET', KEYS[1], new, 'EX', ttl)
	return 1
end
return 0
`)

// Set stores a session hash and registers it in the user's secondary
// index. The TTL is applied to both keys so an abandoned session
// expires from the index along with the hash and we never accumulate
// dangling references.
func (s *RedisSessionStore) Set(ctx context.Context, sessionID string, userID, workspaceID uuid.UUID, ttl time.Duration) error {
	if sessionID == "" {
		return errSessionIDRequired
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
	// The user_sessions SET TTL must never shrink: a short-lived
	// session followed by a long-lived one would otherwise expire
	// the index before the long-lived hash, leaving
	// RevokeAllForUser unable to find it. ExpireNX seeds the TTL on
	// first SAdd when none
	// exists; ExpireGT extends it only when ttl is greater than the
	// current remaining TTL. Together they implement
	// max(current, ttl) without a separate PTTL round-trip.
	pipe.ExpireNX(ctx, ukey, ttl)
	pipe.ExpireGT(ctx, ukey, ttl)
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

// RevokeUser records a per-user revocation cutoff: every token
// issued at or before `at` for the given user in the given workspace
// is considered revoked. AuthMiddleware consults this via IsRevoked
// on every authenticated request, so the next request from any
// replica sees the revocation within one Redis round trip.
//
// The cutoff is stored with a TTL matching the longest plausible JWT
// lifetime (`ttl` argument, normally the API's TokenTTL) — after
// that, no token the cutoff could revoke remains valid, so the key
// can self-clean rather than accumulating per-user state forever.
//
// `at` is rounded to second precision because JWT `iat` is a numeric
// date with second resolution; storing finer precision would let a
// token issued in the same wall-clock second as the revocation
// either slip past or be incorrectly rejected depending on rounding.
// The conservative choice is "any token issued at or before the
// revocation second is revoked", which matches the comparison in
// IsRevoked.
//
// When `ttl` is zero we fall back to a 24-hour default that matches
// middleware.TokenTTL. Storing zero or omitting the EXPIRE would
// leak the key indefinitely.
func (s *RedisSessionStore) RevokeUser(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	// Redis EX is in whole seconds and rejects 0 as an invalid expire
	// time. int64(ttl.Seconds()) truncates anything in (0, 1s) to 0
	// and would have produced a runtime error from the EVAL. Production
	// today only ever passes middleware.TokenTTL (24h) so this is
	// defence-in-depth, but the floor keeps the contract crisp: any
	// positive duration produces a key with at least 1s of TTL.
	ttlSeconds := int64(ttl.Seconds())
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	cutoff := at.UTC().Unix()
	key := userRevokedKey(workspaceID, userID)
	// Atomic max-update via Lua: the script only overwrites the
	// existing cutoff when the new timestamp is strictly greater.
	// This prevents the (rare but real) race where two concurrent
	// revocations land out-of-order and a stale earlier timestamp
	// moves the cutoff backwards, re-validating tokens the earlier
	// revocation intended to reject.
	//
	// EVAL is single-threaded inside Redis, so we get true
	// atomicity without a CAS retry loop. The TTL is set in the
	// same script so a winning write also refreshes the expiry.
	// The script always returns an integer (1 on write, 0 on no-op),
	// so we don't need a redis.Nil guard — any non-nil error here is
	// a transport- or script-level failure.
	if err := revokeUserScript.Run(ctx, s.client, []string{key}, cutoff, ttlSeconds).Err(); err != nil {
		return fmt.Errorf("redis revoke user: %w", err)
	}
	return nil
}

// IsRevoked reports whether the per-user revocation cutoff for the
// given (workspace, user) is set to a value greater than or equal to
// `issuedAt`. If no cutoff exists (redis.Nil), the user has never
// been force-logged-out and the token is treated as valid.
//
// Callers should fail closed on transport errors: a flaky Redis must
// not silently degrade revocation to a no-op. The middleware returns
// 401 on any non-nil error.
//
// The comparison is `iat <= cutoff` so a token issued *in the same
// second* as a revocation is considered revoked. This matches
// RevokeUser's "tokens issued at or before this moment" contract and
// avoids the second-rounding edge case where a JWT issued just before
// logout would otherwise survive on a colocated wall-clock tick.
func (s *RedisSessionStore) IsRevoked(ctx context.Context, workspaceID, userID uuid.UUID, issuedAt time.Time) (bool, error) {
	raw, err := s.client.Get(ctx, userRevokedKey(workspaceID, userID)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, fmt.Errorf("redis is-revoked: %w", err)
	}
	// Parse via a fixed-width integer rather than time.Parse: the
	// stored value is the Unix second from RevokeUser. A malformed
	// value (manual Redis surgery, corrupted memory) fails closed.
	cutoff, perr := parseUnixSeconds(raw)
	if perr != nil {
		return false, fmt.Errorf("parse revoke cutoff: %w", perr)
	}
	return issuedAt.UTC().Unix() <= cutoff, nil
}

// parseUnixSeconds decodes the integer cutoff written by RevokeUser.
// Pulled into a helper so the parse error path stays uniform and so
// the IsRevoked logic reads as a single comparison.
//
// strconv.ParseInt (rather than fmt.Sscanf) is the strict choice:
// Sscanf with "%d" succeeds on inputs like "1234abc" by stopping at
// the first non-digit and silently dropping the trailing garbage,
// which would let a corrupted value like "1700000000.5" parse as
// 1700000000 (off-by-half-a-second) without surfacing the
// corruption. ParseInt rejects any trailing non-numeric bytes
// outright, so IsRevoked fails closed on a malformed key and the
// next RevokeUser overwrites it cleanly via the Lua self-heal path.
func parseUnixSeconds(raw string) (int64, error) {
	return strconv.ParseInt(raw, 10, 64)
}
