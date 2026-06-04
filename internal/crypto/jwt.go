package crypto

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Algorithm names recognised by the KeyManager. These mirror the
// RFC 7518 `alg` values and the jwt_signing_keys.algorithm column.
const (
	AlgES256 = "ES256"
	AlgHS256 = "HS256"
	// AlgAuto signs with ES256 when an active asymmetric key exists,
	// otherwise HS256. Verification accepts both regardless.
	AlgAuto = "auto"
)

// SigningKeyRecord mirrors one jwt_signing_keys row. PrivateKeyPEMEncrypted
// holds the AES-GCM ciphertext (an opaque blob) of the SEC1 EC private
// key PEM; it is decrypted lazily by the KeyManager via the credential
// Codec and is never exposed outside this package.
type SigningKeyRecord struct {
	ID                     uuid.UUID
	WorkspaceID            *uuid.UUID
	Algorithm              string
	PublicKeyPEM           string
	PrivateKeyPEMEncrypted []byte
	IsActive               bool
	CreatedAt              time.Time
	RotatedAt              *time.Time
}

// SigningKeyStore is the persistence boundary for asymmetric signing
// keys. It is an interface so the KeyManager can be unit-tested with
// an in-memory fake (see jwt_test.go) without a live Postgres.
//
// All methods operate on the platform-wide scope (workspace_id IS
// NULL); per-workspace keys are reserved by the schema but not yet
// issued.
type SigningKeyStore interface {
	// ListKeys returns every platform-wide key (active and retired)
	// ordered newest-first. Retired keys are retained so tokens they
	// signed keep verifying until expiry.
	ListKeys(ctx context.Context) ([]SigningKeyRecord, error)
	// Rotate atomically retires the currently-active platform key (if
	// any) and inserts rec as the new active key, in a single
	// transaction so there is never zero or two active keys.
	Rotate(ctx context.Context, rec SigningKeyRecord) error
}

// KeyManager signs and verifies session JWTs. It signs with ES256
// (ECDSA P-256) when an active asymmetric key is loaded, and otherwise
// falls back to HS256 using the symmetric secret. Verification always
// accepts both ES256 (matched by the JWT `kid` header against every
// loaded key, active or retired) and HS256 — so rotating to ES256, or
// rotating one ES256 key to another, never invalidates tokens already
// in the wild.
//
// The set of keys is loaded once at construction and refreshed on
// RotateKey. A KeyManager is safe for concurrent use.
type KeyManager struct {
	store      SigningKeyStore
	codec      *Codec
	hmacSecret string
	algoPref   string

	// reloadMu serializes the whole reload sequence (store read → build
	// → swap) so two concurrent reloaders (e.g. RotateKey and the
	// RefreshLoop tick) cannot interleave such that a reloader holding a
	// stale pre-rotation snapshot swaps in last and clobbers a fresher
	// one. It is held across the DB round-trip; mu (below) is taken only
	// for the brief in-memory swap, so Sign/Parse never block on I/O.
	reloadMu sync.Mutex

	mu sync.RWMutex
	// signingKID / signingKey describe the active ES256 key used for
	// signing. signingKey is nil when no asymmetric key is active (or
	// when algoPref forces HS256), in which case signing uses HMAC.
	signingKID string
	signingKey *ecdsa.PrivateKey
	// verifyKeys maps a key id (kid) to its ES256 public key for
	// verification. Includes retired keys.
	verifyKeys map[string]*ecdsa.PublicKey
}

// NewKeyManager builds a KeyManager and loads the current key set.
// store may be nil (e.g. in tests or deployments with no key table),
// in which case the manager is HS256-only. hmacSecret is the HS256
// fallback secret (JWT_SECRET). algoPref is one of AlgAuto, AlgES256,
// or AlgHS256; an empty value is treated as AlgAuto.
func NewKeyManager(ctx context.Context, store SigningKeyStore, codec *Codec, hmacSecret, algoPref string) (*KeyManager, error) {
	if algoPref == "" {
		algoPref = AlgAuto
	}
	km := &KeyManager{
		store:      store,
		codec:      codec,
		hmacSecret: hmacSecret,
		algoPref:   algoPref,
		verifyKeys: map[string]*ecdsa.PublicKey{},
	}
	if err := km.reload(ctx); err != nil {
		return nil, err
	}
	return km, nil
}

// Algorithm reports the algorithm the manager will use to sign the
// next token: AlgES256 when an active asymmetric key is loaded,
// otherwise AlgHS256.
func (km *KeyManager) Algorithm() string {
	km.mu.RLock()
	defer km.mu.RUnlock()
	if km.signingKey != nil {
		return AlgES256
	}
	return AlgHS256
}

// reload reads every key from the store and rebuilds the verification
// map keyed by kid. Verification keys are always loaded when a store
// is present — independent of algoPref — so that existing ES256 tokens
// keep verifying even when algoPref forces HS256 (e.g. an operator
// rolling signing back to HS256 must not lock out users holding valid
// ES256 tokens). The active ES256 key is selected for signing only
// when algoPref does not force HS256. A nil store leaves the manager
// HS256-only.
func (km *KeyManager) reload(ctx context.Context) error {
	// Serialize the entire read→build→swap so a reloader cannot swap in a
	// snapshot older than one a concurrent reloader already installed.
	// Without this, both callers read the store and build their maps
	// lock-free, and only the final assignment is guarded by mu — letting
	// a stale reader that locks second overwrite a fresher set.
	km.reloadMu.Lock()
	defer km.reloadMu.Unlock()

	verify := map[string]*ecdsa.PublicKey{}
	var signKID string
	var signKey *ecdsa.PrivateKey

	if km.store != nil {
		recs, err := km.store.ListKeys(ctx)
		if err != nil {
			return fmt.Errorf("crypto: list signing keys: %w", err)
		}
		for _, rec := range recs {
			pub, err := parseECPublicKeyPEM(rec.PublicKeyPEM)
			if err != nil {
				return fmt.Errorf("crypto: parse public key %s: %w", rec.ID, err)
			}
			verify[rec.ID.String()] = pub
			if rec.IsActive && signKey == nil && km.algoPref != AlgHS256 {
				priv, err := km.decryptPrivateKey(ctx, rec.PrivateKeyPEMEncrypted)
				if err != nil {
					return fmt.Errorf("crypto: load active private key %s: %w", rec.ID, err)
				}
				signKID = rec.ID.String()
				signKey = priv
			}
		}
	}

	km.mu.Lock()
	km.verifyKeys = verify
	km.signingKID = signKID
	km.signingKey = signKey
	km.mu.Unlock()
	return nil
}

// Reload re-reads the signing-key set from the store, rebuilding the
// verification map and re-selecting the active signing key. It is the
// exported entrypoint the background refresh loop (and tests) use to
// pick up rotations performed by other replicas. A KeyManager with no
// store reloads to an HS256-only state without error. On failure the
// previously-loaded key set is left intact (reload only swaps the
// in-memory maps once the read fully succeeds).
func (km *KeyManager) Reload(ctx context.Context) error {
	return km.reload(ctx)
}

// RefreshLoop periodically calls Reload until ctx is cancelled, so a
// key rotated in on any replica (which writes jwt_signing_keys and
// reloads only itself, see RotateKey) propagates to every other
// replica within one interval — without a restart. This closes the
// multi-replica gap where a token signed by a freshly-rotated key
// would 401 on replicas that had not yet observed the new key's
// public half.
//
// interval must be positive; a non-positive value disables the loop
// (callers gate on JWTKeyRefreshInterval > 0). The loop is resilient:
// a failed Reload (e.g. a transient DB blip) is logged and retried on
// the next tick rather than aborting, so a momentary outage cannot
// permanently freeze the key set. RefreshLoop blocks until ctx is
// done and is intended to run in its own goroutine.
func (km *KeyManager) RefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := km.reload(ctx); err != nil {
				slog.Warn("jwt signing-key auto-refresh failed; retaining existing key set", "err", err)
			}
		}
	}
}

// Sign issues a signed token string for the given claims. It uses
// ES256 with the active key when one is loaded, otherwise HS256.
func (km *KeyManager) Sign(claims jwt.Claims) (string, error) {
	km.mu.RLock()
	signKey := km.signingKey
	signKID := km.signingKID
	km.mu.RUnlock()

	if signKey != nil {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
		tok.Header["kid"] = signKID
		return tok.SignedString(signKey)
	}
	// algoPref == AlgES256 is a hard compliance requirement: refuse to
	// mint HS256 tokens when an operator has demanded asymmetric
	// signing but no active key has been rotated in yet. Silently
	// downgrading here would hand out symmetric tokens to a deployment
	// that believes it is asymmetric-only.
	if km.algoPref == AlgES256 {
		return "", errors.New("crypto: JWT_ALGORITHM=ES256 but no active asymmetric signing key (run POST /api/platform/jwt/rotate first)")
	}
	if km.hmacSecret == "" {
		return "", errors.New("crypto: no signing key and empty HS256 secret")
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(km.hmacSecret))
}

// Parse verifies raw into the supplied claims pointer. ES256 tokens are
// matched by their `kid` header against the loaded verification keys;
// HS256 tokens are verified against the symmetric secret. This is the
// "try ES256, fall back to HS256" behaviour expressed as a single
// keyfunc keyed on the token's own signing method.
func (km *KeyManager) Parse(raw string, into jwt.Claims) (*jwt.Token, error) {
	return jwt.ParseWithClaims(raw, into, func(t *jwt.Token) (interface{}, error) {
		switch t.Method.(type) {
		case *jwt.SigningMethodECDSA:
			kid, _ := t.Header["kid"].(string)
			km.mu.RLock()
			pub, ok := km.verifyKeys[kid]
			km.mu.RUnlock()
			if !ok {
				return nil, fmt.Errorf("crypto: unknown signing key id %q", kid)
			}
			return pub, nil
		case *jwt.SigningMethodHMAC:
			if km.hmacSecret == "" {
				return nil, errors.New("crypto: HS256 token but no secret configured")
			}
			return []byte(km.hmacSecret), nil
		default:
			return nil, fmt.Errorf("crypto: unexpected signing method %v", t.Header["alg"])
		}
	})
}

// RotateKey generates a fresh ES256 keypair, persists it (encrypting
// the private key with the credential codec) as the new active key,
// retires the previously-active key, and reloads the in-memory key set
// so subsequent Sign calls use the new key while Parse still accepts
// tokens signed by the retired one. It returns the new key's public
// metadata (never the private key).
//
// Requires a store: a KeyManager constructed without one (HS256-only)
// returns an error.
func (km *KeyManager) RotateKey(ctx context.Context) (SigningKeyRecord, error) {
	if km.store == nil {
		return SigningKeyRecord{}, errors.New("crypto: cannot rotate without a signing key store")
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return SigningKeyRecord{}, fmt.Errorf("crypto: generate P-256 key: %w", err)
	}
	pubPEM, err := marshalECPublicKeyPEM(&priv.PublicKey)
	if err != nil {
		return SigningKeyRecord{}, err
	}
	privPEM, err := marshalECPrivateKeyPEM(priv)
	if err != nil {
		return SigningKeyRecord{}, err
	}
	encrypted, err := km.codec.Encrypt(ctx, privPEM)
	if err != nil {
		return SigningKeyRecord{}, fmt.Errorf("crypto: encrypt private key: %w", err)
	}

	rec := SigningKeyRecord{
		ID:                     uuid.New(),
		WorkspaceID:            nil,
		Algorithm:              AlgES256,
		PublicKeyPEM:           pubPEM,
		PrivateKeyPEMEncrypted: []byte(encrypted),
		IsActive:               true,
		CreatedAt:              time.Now().UTC(),
	}
	if err := km.store.Rotate(ctx, rec); err != nil {
		return SigningKeyRecord{}, fmt.Errorf("crypto: persist rotated key: %w", err)
	}
	if err := km.reload(ctx); err != nil {
		return SigningKeyRecord{}, err
	}

	// Never hand back the (encrypted) private material to callers.
	rec.PrivateKeyPEMEncrypted = nil
	return rec, nil
}

// decryptPrivateKey opens the stored ciphertext via the credential
// codec and parses the resulting SEC1 EC private key PEM.
func (km *KeyManager) decryptPrivateKey(ctx context.Context, encrypted []byte) (*ecdsa.PrivateKey, error) {
	pemStr, err := km.codec.Decrypt(ctx, string(encrypted))
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return parseECPrivateKeyPEM(pemStr)
}

// marshalECPublicKeyPEM encodes an ECDSA public key as a PKIX/SPKI
// "PUBLIC KEY" PEM block.
func marshalECPublicKeyPEM(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("crypto: marshal public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// marshalECPrivateKeyPEM encodes an ECDSA private key as a SEC1
// "EC PRIVATE KEY" PEM block.
func marshalECPrivateKeyPEM(priv *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("crypto: marshal private key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

func parseECPublicKeyPEM(s string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("crypto: no PEM block in public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("crypto: public key is not ECDSA")
	}
	return pub, nil
}

func parseECPrivateKeyPEM(s string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("crypto: no PEM block in private key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// PostgresSigningKeyStore implements SigningKeyStore against the
// jwt_signing_keys table (migration 034). It operates on the
// platform-wide scope (workspace_id IS NULL).
type PostgresSigningKeyStore struct {
	pool *pgxpool.Pool
}

// NewPostgresSigningKeyStore returns a store backed by pool.
func NewPostgresSigningKeyStore(pool *pgxpool.Pool) *PostgresSigningKeyStore {
	return &PostgresSigningKeyStore{pool: pool}
}

// ListKeys returns all platform-wide keys, newest first.
func (s *PostgresSigningKeyStore) ListKeys(ctx context.Context) ([]SigningKeyRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, algorithm, public_key_pem,
		       private_key_pem_encrypted, is_active, created_at, rotated_at
		FROM jwt_signing_keys
		WHERE workspace_id IS NULL
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SigningKeyRecord
	for rows.Next() {
		var rec SigningKeyRecord
		if err := rows.Scan(
			&rec.ID, &rec.WorkspaceID, &rec.Algorithm, &rec.PublicKeyPEM,
			&rec.PrivateKeyPEMEncrypted, &rec.IsActive, &rec.CreatedAt, &rec.RotatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Rotate retires the active platform key and inserts rec as active, in
// a single transaction.
func (s *PostgresSigningKeyStore) Rotate(ctx context.Context, rec SigningKeyRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE jwt_signing_keys
		SET is_active = FALSE, rotated_at = now()
		WHERE workspace_id IS NULL AND is_active`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO jwt_signing_keys
			(id, workspace_id, algorithm, public_key_pem,
			 private_key_pem_encrypted, is_active, created_at)
		VALUES ($1, NULL, $2, $3, $4, TRUE, $5)`,
		rec.ID, rec.Algorithm, rec.PublicKeyPEM, rec.PrivateKeyPEMEncrypted, rec.CreatedAt,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
