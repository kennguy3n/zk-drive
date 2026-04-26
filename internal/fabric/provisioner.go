// Package fabric integrates ZK Drive with the zk-object-fabric console
// API. Each ZK Drive workspace maps 1:1 to a fabric tenant; this package
// owns the (best-effort) signup-time call that mints a tenant + API
// key pair and stores it in workspace_storage_credentials.
package fabric

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Credentials describes the persisted row in
// workspace_storage_credentials. SecretKey is the *plaintext* secret;
// callers are responsible for encrypting it before storage if a KMS is
// configured (the IdentityDecryptor variant simply round-trips it).
type Credentials struct {
	WorkspaceID          uuid.UUID
	TenantID             string
	AccessKey            string
	SecretKey            string
	Endpoint             string
	Bucket               string
	PlacementPolicyRef   string
	DataResidencyCountry string
}

// SecretEncryptor protects the at-rest secret_key_encrypted column.
// The default implementation is the identity encryptor, suitable for
// local-dev / CI but expected to be swapped for a KMS-backed
// implementation in production.
type SecretEncryptor interface {
	Encrypt(ctx context.Context, plaintext string) (string, error)
}

// IdentityEncryptor returns the input unchanged. Pair with
// storage.IdentityDecryptor.
type IdentityEncryptor struct{}

// Encrypt implements SecretEncryptor.
func (IdentityEncryptor) Encrypt(_ context.Context, plaintext string) (string, error) {
	return plaintext, nil
}

// Provisioner orchestrates tenant creation against zk-object-fabric
// and persistence into workspace_storage_credentials. It is a
// best-effort dependency: signup must succeed even when the fabric
// console is unreachable, so callers log and ignore Provision errors.
type Provisioner struct {
	pool      *pgxpool.Pool
	httpc     *http.Client
	consoleURL string
	bucketTmpl string
	defaultPolicyRef string
	encryptor SecretEncryptor

	// emailSuffix is appended when the provisioner generates a
	// per-workspace email for the tenant signup payload. Defaults to
	// "@zk-drive.local".
	emailSuffix string
}

// Config configures the provisioner. ConsoleURL is the only required
// field; missing fields fall back to sensible defaults.
type Config struct {
	ConsoleURL         string
	BucketTemplate     string
	DefaultPolicyRef   string
	HTTPClient         *http.Client
	Encryptor          SecretEncryptor
	EmailSuffix        string
}

// NewProvisioner builds a Provisioner. When pool is nil, Persist is
// a no-op; when cfg.ConsoleURL is empty, Provision returns an error
// so the signup wiring can detect the deferred state and skip the
// fabric round-trip.
func NewProvisioner(pool *pgxpool.Pool, cfg Config) *Provisioner {
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	enc := cfg.Encryptor
	if enc == nil {
		enc = IdentityEncryptor{}
	}
	bucket := cfg.BucketTemplate
	if bucket == "" {
		bucket = "zk-drive-{tenant}"
	}
	policy := cfg.DefaultPolicyRef
	if policy == "" {
		policy = "b2c_pooled_default"
	}
	suffix := cfg.EmailSuffix
	if suffix == "" {
		suffix = "@zk-drive.local"
	}
	return &Provisioner{
		pool:             pool,
		httpc:            httpc,
		consoleURL:       strings.TrimRight(cfg.ConsoleURL, "/"),
		bucketTmpl:       bucket,
		defaultPolicyRef: policy,
		encryptor:        enc,
		emailSuffix:      suffix,
	}
}

// ErrConsoleNotConfigured is returned when Provision is invoked
// without a console URL. Callers (the signup handler) treat this as
// "fabric integration disabled" and fall back to the static client.
var ErrConsoleNotConfigured = errors.New("fabric: console URL not configured")

// Provision mints a fabric tenant and persists the resulting
// credentials. It is best-effort: callers should log and ignore
// errors so workspace creation still succeeds when the fabric
// console is unreachable.
func (p *Provisioner) Provision(ctx context.Context, workspaceID uuid.UUID, workspaceName string) (*Credentials, error) {
	if p.consoleURL == "" {
		return nil, ErrConsoleNotConfigured
	}
	resp, err := p.signupTenant(ctx, workspaceID, workspaceName)
	if err != nil {
		return nil, err
	}
	creds := &Credentials{
		WorkspaceID:        workspaceID,
		TenantID:           resp.Tenant.ID,
		AccessKey:          resp.AccessKey,
		SecretKey:          resp.SecretKey,
		Endpoint:           p.consoleURL,
		Bucket:             p.bucketFor(resp.Tenant.ID),
		PlacementPolicyRef: p.defaultPolicyRef,
	}
	if resp.Tenant.PlacementDefaultPolicyRef != "" {
		creds.PlacementPolicyRef = resp.Tenant.PlacementDefaultPolicyRef
	}
	if err := p.Persist(ctx, creds); err != nil {
		return creds, fmt.Errorf("persist credentials: %w", err)
	}
	return creds, nil
}

// Persist writes a Credentials row into workspace_storage_credentials.
// It encrypts SecretKey via the configured encryptor before insert.
// When the factory pool is nil this is a no-op so callers can build a
// metadata-only Provisioner for tests.
func (p *Provisioner) Persist(ctx context.Context, c *Credentials) error {
	if p.pool == nil {
		return nil
	}
	enc, err := p.encryptor.Encrypt(ctx, c.SecretKey)
	if err != nil {
		return fmt.Errorf("encrypt secret: %w", err)
	}
	const q = `
INSERT INTO workspace_storage_credentials
    (workspace_id, tenant_id, access_key, secret_key_encrypted, endpoint, bucket, placement_policy_ref, data_residency_country)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (workspace_id) DO UPDATE SET
    tenant_id = EXCLUDED.tenant_id,
    access_key = EXCLUDED.access_key,
    secret_key_encrypted = EXCLUDED.secret_key_encrypted,
    endpoint = EXCLUDED.endpoint,
    bucket = EXCLUDED.bucket,
    placement_policy_ref = EXCLUDED.placement_policy_ref,
    data_residency_country = EXCLUDED.data_residency_country,
    updated_at = now()`
	residency := nullable(c.DataResidencyCountry)
	if _, err := p.pool.Exec(ctx, q, c.WorkspaceID, c.TenantID, c.AccessKey, enc, c.Endpoint, c.Bucket, c.PlacementPolicyRef, residency); err != nil {
		return fmt.Errorf("upsert workspace_storage_credentials: %w", err)
	}
	return nil
}

// LookupTenantID resolves the fabric tenant ID for a workspace.
// Returns ("", ErrNoCredentials) when no row exists so callers can
// distinguish "not provisioned" from a database error.
func (p *Provisioner) LookupTenantID(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	if p.pool == nil {
		return "", ErrNoCredentials
	}
	var tenantID string
	const q = `SELECT tenant_id FROM workspace_storage_credentials WHERE workspace_id = $1`
	if err := p.pool.QueryRow(ctx, q, workspaceID).Scan(&tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoCredentials
		}
		return "", fmt.Errorf("lookup tenant_id: %w", err)
	}
	return tenantID, nil
}

// UpdatePlacement updates the placement policy reference (and
// optional data-residency country) recorded for a workspace.
// Returns ErrNoCredentials when no row exists.
func (p *Provisioner) UpdatePlacement(ctx context.Context, workspaceID uuid.UUID, policyRef, country string) error {
	if p.pool == nil {
		return nil
	}
	const q = `
UPDATE workspace_storage_credentials
   SET placement_policy_ref = $2,
       data_residency_country = $3,
       updated_at = now()
 WHERE workspace_id = $1`
	tag, err := p.pool.Exec(ctx, q, workspaceID, policyRef, nullable(country))
	if err != nil {
		return fmt.Errorf("update placement: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoCredentials
	}
	return nil
}

// UpdateCMK persists the workspace's customer-managed key URI into
// workspace_storage_credentials.cmk_uri. Empty input resets the
// workspace back to the gateway-default key. Returns ErrNoCredentials
// when no row exists for the workspace so the admin handler can
// surface a 404 rather than a silent no-op.
func (p *Provisioner) UpdateCMK(ctx context.Context, workspaceID uuid.UUID, cmkURI string) error {
	if p.pool == nil {
		return nil
	}
	const q = `
UPDATE workspace_storage_credentials
   SET cmk_uri = $2,
       updated_at = now()
 WHERE workspace_id = $1`
	tag, err := p.pool.Exec(ctx, q, workspaceID, strings.TrimSpace(cmkURI))
	if err != nil {
		return fmt.Errorf("update cmk: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoCredentials
	}
	return nil
}

// LookupCMK returns the persisted cmk_uri for a workspace. An empty
// string with a nil error means "row exists, gateway default in
// use". Returns ErrNoCredentials when no credentials row exists.
func (p *Provisioner) LookupCMK(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	if p.pool == nil {
		return "", ErrNoCredentials
	}
	var uri string
	const q = `SELECT cmk_uri FROM workspace_storage_credentials WHERE workspace_id = $1`
	if err := p.pool.QueryRow(ctx, q, workspaceID).Scan(&uri); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoCredentials
		}
		return "", fmt.Errorf("lookup cmk_uri: %w", err)
	}
	return uri, nil
}

// ErrNoCredentials is returned by lookups when no
// workspace_storage_credentials row exists.
var ErrNoCredentials = errors.New("fabric: no credentials for workspace")

func (p *Provisioner) bucketFor(tenantID string) string {
	return strings.ReplaceAll(p.bucketTmpl, "{tenant}", tenantID)
}

// tenantSummary mirrors the subset of zk-object-fabric's
// console.TenantSummary that the provisioner cares about. We keep our
// own type so we do not introduce a cross-repo Go-module dependency.
type tenantSummary struct {
	ID                        string `json:"id"`
	PlacementDefaultPolicyRef string `json:"placementDefaultPolicyRef"`
}

// authResponse mirrors zk-object-fabric's console.AuthResponse.
type authResponse struct {
	Tenant    tenantSummary `json:"tenant"`
	AccessKey string        `json:"accessKey"`
	SecretKey string        `json:"secretKey"`
}

func (p *Provisioner) signupTenant(ctx context.Context, workspaceID uuid.UUID, workspaceName string) (*authResponse, error) {
	password, err := randomPassword(24)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	body := map[string]string{
		"email":      workspaceID.String() + p.emailSuffix,
		"password":   password,
		"tenantName": workspaceName,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpoint, err := url.JoinPath(p.consoleURL, "/api/v1/auth/signup")
	if err != nil {
		return nil, fmt.Errorf("build signup URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call console signup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Drain a small slice of the body for diagnostics; never log it
		// in full because callers may have included credentials.
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("console signup: status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	var out authResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode console signup: %w", err)
	}
	if out.Tenant.ID == "" || out.AccessKey == "" || out.SecretKey == "" {
		return nil, errors.New("console signup: missing fields in response")
	}
	return &out, nil
}

func randomPassword(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
