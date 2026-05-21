// Package storage wraps the AWS SDK v2 S3 presign client to generate
// presigned PUT and GET URLs against a zk-object-fabric gateway endpoint.
// It never transfers file bytes — only brokers URLs.
package storage

import (
	"context"
	"errors"
	"fmt"
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
}

// Client wraps an s3.PresignClient scoped to a single bucket. It only
// generates URLs; the actual PUT / GET bytes flow directly between the
// caller and the zk-object-fabric gateway.
type Client struct {
	bucket  string
	presign *s3.PresignClient
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

	s3Client := s3.New(s3.Options{
		Region:           region,
		Credentials:      creds,
		EndpointResolverV2: staticEndpointResolver{endpoint: cfg.Endpoint},
		UsePathStyle:     true,
	})

	return &Client{
		bucket:  cfg.Bucket,
		presign: s3.NewPresignClient(s3Client),
	}, nil
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

	workspaceID, err := uuid.Parse(parts[0])
	if err != nil || workspaceID != expectedWorkspace || workspaceID.String() != parts[0] {
		return uuid.Nil, ErrInvalidObjectKey
	}
	fileID, err := uuid.Parse(parts[1])
	if err != nil || fileID != expectedFile || fileID.String() != parts[1] {
		return uuid.Nil, ErrInvalidObjectKey
	}
	versionID, err := uuid.Parse(parts[2])
	if err != nil || versionID.String() != parts[2] {
		return uuid.Nil, ErrInvalidObjectKey
	}
	return versionID, nil
}
