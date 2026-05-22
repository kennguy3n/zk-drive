package health

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeStorageProbe is a minimal in-process storageProbe used to
// exercise StorageChecker without bringing up the AWS SDK / a real
// S3 endpoint.
type fakeStorageProbe struct {
	err error
}

func (f *fakeStorageProbe) HealthCheck(_ context.Context) error { return f.err }

func TestStorageCheckerNilProbeReportsOK(t *testing.T) {
	c := NewStorageChecker(nil)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("nil probe should be OK (optional dep), got %v", err)
	}
	if c.Name() != "storage" {
		t.Fatalf("Name(): expected storage, got %q", c.Name())
	}
}

func TestStorageCheckerPropagatesProbeError(t *testing.T) {
	c := NewStorageChecker(&fakeStorageProbe{err: errors.New("403 forbidden")})
	err := c.Check(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "storage health") {
		t.Fatalf("expected wrapped error, got %q", err)
	}
	if !strings.Contains(err.Error(), "403 forbidden") {
		t.Fatalf("expected original cause in error chain, got %q", err)
	}
}

func TestStorageCheckerHappyPath(t *testing.T) {
	c := NewStorageChecker(&fakeStorageProbe{})
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("happy path: expected nil, got %v", err)
	}
}

func TestRedisCheckerNilClientReportsOK(t *testing.T) {
	// Nil client = "REDIS_URL unset, in-memory mode" = not a failure.
	c := NewRedisChecker(nil)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("nil client should be OK (optional dep), got %v", err)
	}
	if c.Name() != "redis" {
		t.Fatalf("Name(): expected redis, got %q", c.Name())
	}
}

func TestNATSCheckerNilConnReportsOK(t *testing.T) {
	// Nil NATS conn = "NATS_URL unset, post-upload jobs disabled" =
	// not a readiness failure.
	c := NewNATSChecker(nil)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("nil conn should be OK (optional dep), got %v", err)
	}
	if c.Name() != "nats" {
		t.Fatalf("Name(): expected nats, got %q", c.Name())
	}
}

func TestPostgresCheckerNilPoolErrors(t *testing.T) {
	c := NewPostgresChecker(nil)
	if err := c.Check(context.Background()); err == nil {
		t.Fatalf("nil pool should error (postgres is required)")
	}
	if c.Name() != "postgres" {
		t.Fatalf("Name(): expected postgres, got %q", c.Name())
	}
}
