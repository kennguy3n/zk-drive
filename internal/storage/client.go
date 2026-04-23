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
