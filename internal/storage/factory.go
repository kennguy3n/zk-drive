package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CredentialDecryptor turns the at-rest secret_key_encrypted column
// back into the raw S3 secret needed to sign presigned URLs. Callers
// supply their own implementation (KMS, AES-GCM with a static key,
// etc.); the default identity decryptor used by ClientFactory treats
// the stored value as plaintext, which is appropriate for local-dev
// and CI but should be replaced in production.
type CredentialDecryptor interface {
	Decrypt(ctx context.Context, ciphertext string) (string, error)
}

// IdentityDecryptor is the default CredentialDecryptor: it returns
// the input unchanged. Suitable for local-dev / CI; production wires
// a KMS-backed implementation.
type IdentityDecryptor struct{}

// Decrypt implements CredentialDecryptor.
func (IdentityDecryptor) Decrypt(_ context.Context, ciphertext string) (string, error) {
	return ciphertext, nil
}

// ClientFactory resolves an S3 presign client per workspace. It first
// looks up workspace_storage_credentials and, if present, builds a
// workspace-specific client; otherwise it returns the static fallback
// client (typically derived from S3_* env vars). Resolved clients are
// cached in a sync.Map so repeated calls in the same process do not
// re-issue queries or rebuild SDK plumbing.
//
// The factory is safe for concurrent use.
type ClientFactory struct {
	pool       *pgxpool.Pool
	fallback   *Client
	decryptor  CredentialDecryptor
	region     string
	cache      sync.Map // workspaceID -> *Client
}

// NewClientFactory builds a factory backed by the supplied pool. When
// pool is nil, ForWorkspace always returns the fallback client. When
// fallback is nil and no row is found, ForWorkspace returns
// ErrNoCredentials so callers can surface a 501 Not Implemented.
func NewClientFactory(pool *pgxpool.Pool, fallback *Client, decryptor CredentialDecryptor) *ClientFactory {
	if decryptor == nil {
		decryptor = IdentityDecryptor{}
	}
	return &ClientFactory{
		pool:      pool,
		fallback:  fallback,
		decryptor: decryptor,
		region:    DefaultRegion,
	}
}

// WithRegion overrides the AWS region tag used when minting per-workspace
// clients. The default is DefaultRegion.
func (f *ClientFactory) WithRegion(region string) *ClientFactory {
	if region != "" {
		f.region = region
	}
	return f
}

// Fallback returns the static client the factory was constructed with.
// Callers that operate outside a workspace context (e.g. background
// scans across many tenants) can use this directly; everyone else
// should prefer ForWorkspace.
func (f *ClientFactory) Fallback() *Client { return f.fallback }

// ErrNoCredentials is returned by ForWorkspace when the factory has
// neither a per-workspace row nor a static fallback.
var ErrNoCredentials = errors.New("storage: no credentials available for workspace")

// ForWorkspace resolves the client for workspaceID. It returns the
// fallback client when no row exists.
func (f *ClientFactory) ForWorkspace(ctx context.Context, workspaceID uuid.UUID) (*Client, error) {
	if cached, ok := f.cache.Load(workspaceID); ok {
		return cached.(*Client), nil
	}

	cfg, ok, err := f.lookup(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		if f.fallback == nil {
			return nil, ErrNoCredentials
		}
		return f.fallback, nil
	}

	client, err := NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("storage: build workspace client: %w", err)
	}
	// LoadOrStore handles a benign race where two callers resolve the
	// same workspace concurrently — only one client wins, the other is
	// discarded.
	actual, _ := f.cache.LoadOrStore(workspaceID, client)
	return actual.(*Client), nil
}

// Invalidate drops the cached client for workspaceID so the next
// ForWorkspace call re-reads the row. Used after admin endpoints
// rotate credentials or change placement policies.
func (f *ClientFactory) Invalidate(workspaceID uuid.UUID) {
	f.cache.Delete(workspaceID)
}

func (f *ClientFactory) lookup(ctx context.Context, workspaceID uuid.UUID) (Config, bool, error) {
	if f.pool == nil {
		return Config{}, false, nil
	}
	const q = `
SELECT access_key, secret_key_encrypted, endpoint, bucket
FROM workspace_storage_credentials
WHERE workspace_id = $1`
	var (
		accessKey, secretEncrypted, endpoint, bucket string
	)
	if err := f.pool.QueryRow(ctx, q, workspaceID).Scan(&accessKey, &secretEncrypted, &endpoint, &bucket); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Config{}, false, nil
		}
		return Config{}, false, fmt.Errorf("storage: load workspace credentials: %w", err)
	}
	secret, err := f.decryptor.Decrypt(ctx, secretEncrypted)
	if err != nil {
		return Config{}, false, fmt.Errorf("storage: decrypt workspace secret: %w", err)
	}
	return Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secret,
		Region:    f.region,
	}, true, nil
}
