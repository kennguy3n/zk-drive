package platform

import (
	"context"
	"crypto/rand"
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

// A platform key is "pk_<lookup><secret>": a fixed-width, non-secret
// lookup id followed by the secret. The lookup id is stored verbatim
// (indexed, UNIQUE) so Authenticate selects the single candidate row
// in O(1) instead of scanning and bcrypt-comparing every active key;
// only the matched row's hash is then bcrypt-verified. The secret
// carries the security — the lookup id is just a selector.
//
//   apiKeyLookupBytes  12 bytes (96 bits)  -> 16 base64url chars
//   apiKeySecretBytes  24 bytes (192 bits) -> 32 base64url chars
//
// Both base64url-encode without padding, so the lookup is always the
// first apiKeyLookupLen characters after the prefix and parsing needs
// no delimiter (base64url's alphabet itself contains '_').
const (
	apiKeyLookupBytes = 12
	apiKeySecretBytes = 24
)

// apiKeyLookupLen is the encoded width of the lookup id (16). Computed
// from the encoding so it tracks apiKeyLookupBytes.
var apiKeyLookupLen = base64.RawURLEncoding.EncodedLen(apiKeyLookupBytes)

// dummyAPIKeyHash is bcrypt-compared when no candidate row is found so
// a missing lookup id costs the same wall-clock time as a wrong secret
// (a present lookup id is not itself a secret, but equalizing keeps the
// auth path free of an obvious timing oracle).
var dummyAPIKeyHash, _ = bcrypt.GenerateFromPassword([]byte("platform-api-key-timing-equalizer"), bcrypt.DefaultCost)

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

// generateAPIKey returns a fresh plaintext key "pk_<lookup><secret>"
// plus the lookup id to persist alongside the hash. Only the caller
// (APIKeyStore.Create) ever sees the plaintext; the store immediately
// bcrypt-hashes it for persistence.
func generateAPIKey() (plaintext, lookup string, err error) {
	lookupBuf := make([]byte, apiKeyLookupBytes)
	if _, err := rand.Read(lookupBuf); err != nil {
		return "", "", fmt.Errorf("platform: read random: %w", err)
	}
	secretBuf := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(secretBuf); err != nil {
		return "", "", fmt.Errorf("platform: read random: %w", err)
	}
	lookup = base64.RawURLEncoding.EncodeToString(lookupBuf)
	secret := base64.RawURLEncoding.EncodeToString(secretBuf)
	return APIKeyPrefix + lookup + secret, lookup, nil
}

// parseAPIKeyLookup extracts the non-secret lookup id from a presented
// token. It returns false for anything that is not a well-formed
// "pk_<lookup><secret>" (wrong prefix, or too short to carry both the
// lookup id and at least one secret character).
func parseAPIKeyLookup(presented string) (string, bool) {
	if !strings.HasPrefix(presented, APIKeyPrefix) {
		return "", false
	}
	rest := presented[len(APIKeyPrefix):]
	if len(rest) <= apiKeyLookupLen {
		return "", false
	}
	return rest[:apiKeyLookupLen], true
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
		return nil, "", fmt.Errorf("%w: api key label is required", ErrInvalidArgument)
	}
	if permissions == nil {
		permissions = []string{}
	}
	plaintext, lookup, err := generateAPIKey()
	if err != nil {
		return nil, "", err
	}
	hash, err := hashAPIKey(plaintext)
	if err != nil {
		return nil, "", err
	}
	const q = `
INSERT INTO platform_api_keys (key_hash, lookup_id, label, permissions)
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
// The non-secret lookup id embedded in the token selects the single
// candidate row via the UNIQUE index, so authentication is O(1) in the
// number of keys (no full scan) and only that row's bcrypt hash is
// verified. When no row matches the lookup id a dummy bcrypt compare is
// still performed so the "unknown key" path costs roughly the same as
// the "wrong secret" path.
func (s *APIKeyStore) Authenticate(ctx context.Context, presented string) (*APIKey, error) {
	presented = strings.TrimSpace(presented)
	lookup, ok := parseAPIKeyLookup(presented)
	if !ok {
		return nil, ErrAPIKeyInvalid
	}
	const q = `
SELECT id, key_hash, label, permissions, created_at, last_used_at, revoked_at
FROM platform_api_keys
WHERE lookup_id = $1 AND revoked_at IS NULL`
	var (
		k    APIKey
		hash []byte
	)
	err := s.pool.QueryRow(ctx, q, lookup).Scan(
		&k.ID, &hash, &k.Label, &k.Permissions, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Equalize timing with the matched path.
			_ = bcrypt.CompareHashAndPassword(dummyAPIKeyHash, []byte(presented))
			return nil, ErrAPIKeyInvalid
		}
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
