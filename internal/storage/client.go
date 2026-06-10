// Package storage wraps the AWS SDK v2 S3 presign client to generate
// presigned PUT and GET URLs against a zk-object-fabric gateway endpoint.
// It never transfers file bytes — only brokers URLs.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
	"github.com/google/uuid"
)

// DefaultRegion is the region handed to the AWS SDK. zk-object-fabric
// ignores the region in the signing key but the SDK requires a non-empty
// value, so we pin a conventional default and let callers override.
const DefaultRegion = "us-east-1"

// DefaultPresignExpiry is the validity window for presigned URLs when the
// caller does not specify one.
const DefaultPresignExpiry = 15 * time.Minute

// Config describes the parameters needed to talk to a zk-object-fabric S3
// gateway. Region is optional and defaults to DefaultRegion.
type Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	// HTTPClient, when non-nil, overrides the AWS SDK's default HTTP
	// client for this client's transport. The trusted data plane
	// leaves it nil (SDK default). It exists so a caller handling an
	// UNTRUSTED, externally-supplied endpoint — today only the
	// first-boot setup wizard's connection tester — can inject a
	// transport with an SSRF-guarded dialer without that guard
	// leaking onto, or slowing, the operator-configured data-plane
	// client.
	HTTPClient aws.HTTPClient
}

// Client wraps an s3.PresignClient scoped to a single bucket. It only
// generates URLs; the actual PUT / GET bytes flow directly between the
// caller and the zk-object-fabric gateway.
//
// The underlying *s3.Client is also retained so the API server can
// reach the gateway directly for health-check purposes (HeadBucket),
// which exercises both network reachability and credential validity
// in one call — something the presign path cannot do because it
// signs URLs locally without contacting the gateway.
type Client struct {
	bucket  string
	s3      *s3.Client
	presign *s3.PresignClient
	// opRate tracks a rolling-window error rate over the client's
	// server-side direct operations (HealthCheck / Put / Get / Delete
	// / List) for the admin health dashboard. Presign calls are NOT
	// recorded: they never touch the gateway, so they carry no
	// reachability signal. Always non-nil after NewClient.
	rate *opRate
}

// staticEndpointResolver forces every S3 operation to target the configured
// zk-object-fabric gateway, regardless of the region the SDK selects.
type staticEndpointResolver struct {
	endpoint string
}

// ResolveEndpoint implements s3.EndpointResolverV2.
func (r staticEndpointResolver) ResolveEndpoint(ctx context.Context, params s3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	u, err := url.Parse(r.endpoint)
	if err != nil {
		return smithyendpoints.Endpoint{}, fmt.Errorf("storage: invalid endpoint %q: %w", r.endpoint, err)
	}
	return smithyendpoints.Endpoint{URI: *u}, nil
}

// NewClient builds an S3 presign client targeting the zk-object-fabric
// gateway at cfg.Endpoint. It returns an error if required fields are
// missing so callers fail fast at startup.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("storage: endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("storage: bucket is required")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("storage: access key and secret key are required")
	}
	region := cfg.Region
	if strings.TrimSpace(region) == "" {
		region = DefaultRegion
	}

	creds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""))

	opts := s3.Options{
		Region:             region,
		Credentials:        creds,
		EndpointResolverV2: staticEndpointResolver{endpoint: cfg.Endpoint},
		UsePathStyle:       true,
	}
	if cfg.HTTPClient != nil {
		opts.HTTPClient = cfg.HTTPClient
	}
	s3Client := s3.New(opts)

	return &Client{
		bucket:  cfg.Bucket,
		s3:      s3Client,
		presign: s3.NewPresignClient(s3Client),
		rate:    newOpRate(),
	}, nil
}

// recordOp feeds the result of a server-side direct operation into the
// rolling error-rate window. Nil-safe on the receiver's rate field so
// a Client constructed outside NewClient (none today, but defensive)
// never panics.
func (c *Client) recordOp(err error) {
	if c == nil || c.rate == nil {
		return
	}
	c.rate.record(err != nil)
}

// RecentErrorStats returns the trailing-window summary of server-side
// direct operations for the admin health dashboard. A client that has
// performed no operations (or was not constructed via NewClient)
// returns a zero-Total summary, which the dashboard renders as "no
// recent activity" rather than an error.
func (c *Client) RecentErrorStats() OpStats {
	if c == nil || c.rate == nil {
		return OpStats{Window: opRateWindow}
	}
	return c.rate.stats()
}

// HealthCheck verifies the storage backend is reachable and the
// configured credentials still authorise access to the bucket. It
// issues a HeadBucket against the configured zk-object-fabric
// endpoint with the supplied context's deadline.
//
// Returns nil when the bucket responds successfully. Returns the
// wrapped error otherwise — callers (e.g. /readyz) should treat any
// non-nil result as "not ready" since presigned URLs minted while
// the gateway is unreachable would 5xx from the client side.
//
// IAM requirements: HeadBucket maps to the s3:ListBucket action in
// AWS IAM (per the AWS S3 API reference) and the bucket-level READ
// capability in Ceph RGW / MinIO. A presign-only credential scoped
// only to s3:GetObject + s3:PutObject is NOT sufficient — it will
// return 403 here and /readyz will then report storage as failed.
// When provisioning IAM for production, grant s3:ListBucket on the
// bucket ARN (not the object ARN) alongside the existing presign
// permissions. Cost-wise HeadBucket is a Tier-1 (cheap) request in
// AWS pricing and is well within the free tier even for sub-second
// readiness-probe cadence.
func (c *Client) HealthCheck(ctx context.Context) error {
	if c == nil || c.s3 == nil {
		return errors.New("storage: client not initialised")
	}
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	c.recordOp(err)
	if err != nil {
		return fmt.Errorf("storage health check: %w", err)
	}
	return nil
}

// GenerateUploadURL returns a presigned PUT URL for the given object key.
// The contentType, if non-empty, is folded into the signed headers so
// clients can reuse the URL only when they present the same Content-Type.
func (c *Client) GenerateUploadURL(ctx context.Context, objectKey, contentType string, expiry time.Duration) (string, error) {
	if strings.TrimSpace(objectKey) == "" {
		return "", errors.New("storage: object key is required")
	}
	if expiry <= 0 {
		expiry = DefaultPresignExpiry
	}
	in := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	}
	if strings.TrimSpace(contentType) != "" {
		in.ContentType = aws.String(contentType)
	}
	req, err := c.presign.PresignPutObject(ctx, in, func(opts *s3.PresignOptions) {
		opts.Expires = expiry
	})
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return req.URL, nil
}

// GenerateDownloadURL returns a presigned GET URL for the given object key.
func (c *Client) GenerateDownloadURL(ctx context.Context, objectKey string, expiry time.Duration) (string, error) {
	if strings.TrimSpace(objectKey) == "" {
		return "", errors.New("storage: object key is required")
	}
	if expiry <= 0 {
		expiry = DefaultPresignExpiry
	}
	req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = expiry
	})
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return req.URL, nil
}

// DeleteObject removes a single object from the bucket. Used by the
// orphan-object GC reconciler to reclaim storage for presigned PUTs
// the client completed but never confirmed (api/drive/upload.go:
// ConfirmUpload rejection path leaves the S3 object stranded; the
// pending_upload_object_key column records the key so this delete
// can target it directly without a ListObjects scan).
//
// The S3 DeleteObject operation is idempotent: deleting a key that
// no longer exists returns 204 No Content rather than an error, so
// callers don't need to pre-check existence. Versioned buckets
// would respond with a delete-marker, but zk-object-fabric tenants
// for ZK Drive are not version-enabled (object versioning happens
// at the file_versions row level, not the S3 layer), so a single
// DeleteObject permanently reclaims the bytes.
func (c *Client) DeleteObject(ctx context.Context, objectKey string) error {
	if c == nil || c.s3 == nil {
		return errors.New("storage: client not initialised")
	}
	if strings.TrimSpace(objectKey) == "" {
		return errors.New("storage: object key is required")
	}
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	})
	c.recordOp(err)
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

// PutObject uploads body to objectKey with the supplied contentType.
// Unlike GenerateUploadURL (which signs a URL for the CLIENT to PUT),
// this method is used when the API server itself is the producer —
// e.g. the audit-log archiver writing JSONL.gz blobs to the cold tier,
// or any future server-side direct write.
//
// The request is bounded by the supplied context (callers pass a
// timeout). Idempotent at the S3 layer: re-uploading to the same key
// overwrites the previous object atomically (zk-object-fabric tenants
// are not versioned for ZK Drive), so retrying after a transient
// network failure is safe. For idempotent batch writes that must NOT
// collide on retry, the caller should construct a unique key per
// attempt (the audit archiver uses a UUID suffix).
func (c *Client) PutObject(ctx context.Context, objectKey, contentType string, body []byte) error {
	if c == nil || c.s3 == nil {
		return errors.New("storage: client not initialised")
	}
	if strings.TrimSpace(objectKey) == "" {
		return errors.New("storage: object key is required")
	}
	in := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
		Body:   bytes.NewReader(body),
	}
	if strings.TrimSpace(contentType) != "" {
		in.ContentType = aws.String(contentType)
	}
	_, err := c.s3.PutObject(ctx, in)
	c.recordOp(err)
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// GetObject reads the bytes at objectKey. Used by the audit-restore
// CLI to read back archived JSONL.gz blobs. Returns the full body in
// memory — callers should keep individual archive object sizes
// reasonable (the archiver naturally bounds them at one month per
// workspace). NotFound surfaces as a wrapped error so callers can
// errors.As against *s3types.NoSuchKey when they want to distinguish
// "no archive for this period" from a real I/O failure.
func (c *Client) GetObject(ctx context.Context, objectKey string) ([]byte, error) {
	if c == nil || c.s3 == nil {
		return nil, errors.New("storage: client not initialised")
	}
	if strings.TrimSpace(objectKey) == "" {
		return nil, errors.New("storage: object key is required")
	}
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	})
	c.recordOp(err)
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", objectKey, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read object body: %w", err)
	}
	return data, nil
}

// ListObjects enumerates every object whose key starts with prefix
// and invokes fn for each one. fn receiving a non-nil error short-
// circuits the iteration. Paginated under the hood so a prefix
// containing millions of keys still streams safely.
//
// Used by the audit-restore CLI to discover every archive object for
// a workspace's history without round-tripping through Postgres —
// the cold tier is the source of truth when audit_log_archive_runs
// entries get pruned (which we don't do today but may add for very
// long retention windows).
func (c *Client) ListObjects(ctx context.Context, prefix string, fn func(key string, size int64) error) error {
	if c == nil || c.s3 == nil {
		return errors.New("storage: client not initialised")
	}
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		c.recordOp(err)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			if err := fn(*obj.Key, size); err != nil {
				return err
			}
		}
	}
	return nil
}

// Bucket returns the bucket this client is scoped to.
func (c *Client) Bucket() string { return c.bucket }

// NewObjectKey returns the S3 key used for a specific file version. Keys
// are namespaced by workspace so gateway-level ACLs (or future prefix-based
// policies) can enforce tenant isolation, and by version so re-uploads do
// not clobber prior content.
func NewObjectKey(workspaceID, fileID, versionID uuid.UUID) string {
	return fmt.Sprintf("%s/%s/%s", workspaceID.String(), fileID.String(), versionID.String())
}

// ErrInvalidObjectKey is returned by ValidateObjectKey when a
// client-supplied object_key does not match the canonical
// `<workspace_uuid>/<file_uuid>/<version_uuid>` shape produced by
// NewObjectKey. The error message intentionally does not leak the
// reason so a probing client cannot tune its attack to the validator.
var ErrInvalidObjectKey = errors.New("invalid object_key")

// ValidateObjectKey enforces that a client-supplied object_key is the
// canonical three-UUID form generated by NewObjectKey, scoped to the
// expected workspace and file. It rejects path-traversal sequences
// ("..", "."), embedded null bytes, backslashes, empty segments, and
// keys whose UUIDs do not parse — defending the ConfirmUpload code
// path against a client that forges or tampers with the key it
// received from UploadURL.
//
// The function deliberately uses strict equality on the workspace
// and file UUIDs rather than a HasPrefix scan, so a key whose prefix
// matches but whose suffix contains traversal characters is rejected
// outright instead of being trimmed and re-checked.
//
// Each segment must additionally be the *exact* canonical lowercase
// UUID string that `uuid.UUID.String()` produces — `uuid.Parse`
// accepts uppercase / braced / urn:uuid / hyphenless forms which all
// decode to the same UUID but round-trip to a *different* S3 key,
// breaking case-sensitive downloads. NewObjectKey only ever emits
// the lowercase canonical form, so we reject everything else.
//
// On success, the parsed version UUID is returned so callers can
// thread it into downstream metadata (FileVersion.ID, audit logs)
// without re-parsing the key.
func ValidateObjectKey(key string, expectedWorkspace, expectedFile uuid.UUID) (uuid.UUID, error) {
	// NUL bytes and backslashes have no legitimate place in an S3
	// object key generated by NewObjectKey. Reject early so the
	// per-segment loop below never has to reason about them.
	if strings.ContainsAny(key, "\x00\\") {
		return uuid.Nil, ErrInvalidObjectKey
	}

	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return uuid.Nil, ErrInvalidObjectKey
	}

	for _, p := range parts {
		// Empty segments arise from leading/trailing/duplicate
		// slashes ("//", "/foo/", "foo//bar") — all forbidden in
		// the canonical form. Dot segments (".", "..") are the
		// classic path-traversal vector and never appear in a
		// UUID, so reject them defensively even though uuid.Parse
		// would also fail below.
		if p == "" || p == "." || p == ".." {
			return uuid.Nil, ErrInvalidObjectKey
		}
	}

	// All three segments must be canonical, non-nil UUIDs. uuid.Nil
	// (00000000-0000-0000-0000-000000000000) is a valid parse target
	// but a forbidden value here: insertVersionTx and CreateFile
	// treat v.ID == uuid.Nil as "please mint a fresh UUID", which
	// would silently overwrite the pinned versionID and break the
	// "DB row id matches the version segment of the object_key"
	// invariant ConfirmUpload now relies on. UploadURL only ever
	// emits non-nil UUIDs, so any caller submitting uuid.Nil is
	// tampering and must be rejected — fail closed.
	workspaceID, err := uuid.Parse(parts[0])
	if err != nil || workspaceID == uuid.Nil || workspaceID != expectedWorkspace || workspaceID.String() != parts[0] {
		return uuid.Nil, ErrInvalidObjectKey
	}
	fileID, err := uuid.Parse(parts[1])
	if err != nil || fileID == uuid.Nil || fileID != expectedFile || fileID.String() != parts[1] {
		return uuid.Nil, ErrInvalidObjectKey
	}
	versionID, err := uuid.Parse(parts[2])
	if err != nil || versionID == uuid.Nil || versionID.String() != parts[2] {
		return uuid.Nil, ErrInvalidObjectKey
	}
	return versionID, nil
}
