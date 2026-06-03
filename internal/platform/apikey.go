package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// APIKeyPrefix is the human-recognisable prefix every platform API
// key carries (mirrors Stripe's `sk_` / `pk_` convention). The
// platform-auth middleware rejects anything that does not start with
// it before paying the cost of a bcrypt comparison, and operators can
// grep logs / secret stores for the prefix.
const APIKeyPrefix = "pk_"

// apiKeyRandomBytes is the entropy of the random portion of a key.
// 32 bytes (256 bits) base64url-encodes to 43 characters; with the
// prefix the whole token stays well under bcrypt's 72-byte input cap.
const apiKeyRandomBytes = 32

// Coarse capability strings stored in platform_api_keys.permissions
// and checked by the platform-auth middleware. Kept as plain strings
// (not an enum) so adding a capability is a code-only change.
const (
	PermTenantRead       = "tenant:read"
	PermTenantWrite      = "tenant:write"
	PermTenantSuspend    = "tenant:suspend"
	PermBillingReconcile = "billing:reconcile"
	PermAlertsRead       = "alerts:read"
	PermAlertsWrite      = "alerts:write"
	PermKeysManage       = "keys:manage"
)

// ErrAPIKeyInvalid is returned by Authenticate when the presented
// token does not match any active key (wrong prefix, unknown key, or
// revoked). Callers map this to 401 Unauthorized. It deliberately
// does not distinguish the failure modes so a caller probing keys
// cannot tell "no such key" from "revoked key".
var ErrAPIKeyInvalid = errors.New("platform: invalid api key")

// APIKey is the metadata view of a platform_api_keys row. The bcrypt
// hash and the plaintext key are never part of this struct — the
// plaintext is returned exactly once at creation time (see
// APIKeyStore.Create) and the hash never leaves the database layer.
type APIKey struct {
	ID          uuid.UUID  `json:"id"`
	Label       string     `json:"label"`
	Permissions []string   `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// HasPermission reports whether the key carries the given capability.
// A key with the PermKeysManage capability is NOT implicitly granted
// the others — permissions are flat and explicit so the principle of
// least privilege holds for narrowly-scoped automation keys.
func (k *APIKey) HasPermission(permission string) bool {
	if k == nil {
		return false
	}
	for _, p := range k.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

// generateAPIKey returns a fresh plaintext key in the form
// "pk_<base64url-no-padding>". Only the caller (APIKeyStore.Create)
// ever sees the plaintext; the store immediately bcrypt-hashes it for
// persistence.
func generateAPIKey() (string, error) {
	buf := make([]byte, apiKeyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("platform: read random: %w", err)
	}
	return APIKeyPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashAPIKey bcrypt-hashes a plaintext key. bcrypt salts internally so
// two keys with the same value get distinct hashes; verification is
// done via bcrypt.CompareHashAndPassword in Authenticate.
func hashAPIKey(plaintext string) ([]byte, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("platform: hash api key: %w", err)
	}
	return h, nil
}

// lookupDigest derives the deterministic SHA-256 digest stored in
// platform_api_keys.key_lookup. Unlike the bcrypt hash (salted, so it
// cannot be looked up), the digest is a stable function of the
// plaintext, so Authenticate can fetch the single matching row by
// digest instead of scanning every key. This is safe to index and
// equality-match on because the plaintext carries 256 bits of entropy,
// making the digest effectively unguessable; the bcrypt hash remains
// the actual verifier.
func lookupDigest(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// APIKeyStore persists and authenticates platform API keys against
// the platform_api_keys table.
type APIKeyStore struct {
	pool *pgxpool.Pool
}

// NewAPIKeyStore wraps pool in an APIKeyStore.
func NewAPIKeyStore(pool *pgxpool.Pool) *APIKeyStore {
	return &APIKeyStore{pool: pool}
}

// Create mints a new key, stores its bcrypt hash, and returns both the
// row metadata and the one-time plaintext. The plaintext is the ONLY
// time the caller can read the usable key — it is never recoverable
// afterwards. Permissions are stored verbatim; an empty slice yields a
// key that authenticates but carries no capabilities (useful as a
// placeholder before an operator grants scopes).
func (s *APIKeyStore) Create(ctx context.Context, label string, permissions []string) (*APIKey, string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, "", errors.New("platform: api key label is required")
	}
	if permissions == nil {
		permissions = []string{}
	}
	plaintext, err := generateAPIKey()
	if err != nil {
		return nil, "", err
	}
	hash, err := hashAPIKey(plaintext)
	if err != nil {
		return nil, "", err
	}
	lookup := lookupDigest(plaintext)
	const q = `
INSERT INTO platform_api_keys (key_hash, key_lookup, label, permissions)
VALUES ($1, $2, $3, $4)
RETURNING id, label, permissions, created_at, last_used_at, revoked_at`
	key := &APIKey{}
	if err := s.pool.QueryRow(ctx, q, hash, lookup, label, permissions).Scan(
		&key.ID, &key.Label, &key.Permissions, &key.CreatedAt, &key.LastUsedAt, &key.RevokedAt,
	); err != nil {
		return nil, "", fmt.Errorf("platform: insert api key: %w", err)
	}
	return key, plaintext, nil
}

// List returns all keys (redacted — never the hash) newest first,
// including revoked keys so the admin UI can show revocation history.
func (s *APIKeyStore) List(ctx context.Context) ([]APIKey, error) {
	const q = `
SELECT id, label, permissions, created_at, last_used_at, revoked_at
FROM platform_api_keys
ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("platform: list api keys: %w", err)
	}
	defer rows.Close()
	out := make([]APIKey, 0)
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Label, &k.Permissions, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("platform: scan api key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("platform: iterate api keys: %w", err)
	}
	return out, nil
}

// Revoke soft-revokes a key by stamping revoked_at. Idempotent: a
// second revoke leaves the original timestamp untouched. Returns
// ErrNotFound when no key with the id exists.
func (s *APIKeyStore) Revoke(ctx context.Context, id uuid.UUID) error {
	const q = `
UPDATE platform_api_keys
SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("platform: revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate validates a presented token against the active keys and
// returns the matching key's metadata. It returns ErrAPIKeyInvalid for
// any non-match (bad prefix, unknown, revoked). On success it
// best-effort refreshes last_used_at.
//
// The presented token's deterministic SHA-256 digest selects at most
// ONE candidate row through the unique key_lookup index, so the cost is
// a single indexed fetch plus a single bcrypt comparison regardless of
// how many keys exist. This bounds the work an unauthenticated caller
// can force per request (the previous linear scan let a bogus token
// trigger one bcrypt hash per stored key). The bcrypt compare on the
// matched row is retained as defense-in-depth: the digest only selects
// the candidate, the salted bcrypt hash is still the verifier.
func (s *APIKeyStore) Authenticate(ctx context.Context, presented string) (*APIKey, error) {
	presented = strings.TrimSpace(presented)
	if !strings.HasPrefix(presented, APIKeyPrefix) {
		return nil, ErrAPIKeyInvalid
	}
	const q = `
SELECT id, key_hash, label, permissions, created_at, last_used_at, revoked_at
FROM platform_api_keys
WHERE key_lookup = $1 AND revoked_at IS NULL`
	var (
		k    APIKey
		hash []byte
	)
	err := s.pool.QueryRow(ctx, q, lookupDigest(presented)).Scan(
		&k.ID, &hash, &k.Label, &k.Permissions, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAPIKeyInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("platform: load api key: %w", err)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(presented)) != nil {
		return nil, ErrAPIKeyInvalid
	}
	s.touchLastUsed(ctx, k.ID)
	return &k, nil
}

// touchLastUsed refreshes last_used_at. Best-effort: a failure here
// must not fail the request the caller already authenticated, so the
// error is swallowed (the column is observability, not correctness).
func (s *APIKeyStore) touchLastUsed(ctx context.Context, id uuid.UUID) {
	_, _ = s.pool.Exec(ctx, `UPDATE platform_api_keys SET last_used_at = now() WHERE id = $1`, id)
}
