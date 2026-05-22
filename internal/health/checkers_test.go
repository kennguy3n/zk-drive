package health

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// fakeStorageProbe is a minimal in-package fake used to exercise
// StorageChecker.Check error/happy paths without bringing up the
// AWS SDK or a real S3 endpoint. Tests construct &StorageChecker{}
// directly (legal in-package) so we don't widen the public API just
// to swap the probe.
type fakeStorageProbe struct {
	err error
}

func (f *fakeStorageProbe) HealthCheck(_ context.Context) error { return f.err }

// TestNewStorageCheckerNilClientSurvivesTypedNilTrap pins the
// architectural invariant that distinguishes NewStorageChecker from
// a naive interface-typed parameter: passing a nil *storage.Client
// must be normalised to a genuine nil-interface field so the
// Check-time short-circuit fires. Without the constructor
// normalisation, Go would wrap the nil pointer in a non-nil
// interface and Check would invoke HealthCheck on a nil receiver,
// returning the "client not initialised" error and breaking
// readiness for every deployment without S3 configured.
func TestNewStorageCheckerNilClientSurvivesTypedNilTrap(t *testing.T) {
	c := NewStorageChecker(nil)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("nil client should short-circuit to OK, got %v", err)
	}
	if c.probe != nil {
		t.Fatalf("expected nil interface probe, got %T(%v)", c.probe, c.probe)
	}
	if c.Name() != "storage" {
		t.Fatalf("Name(): expected storage, got %q", c.Name())
	}
}

// TestNewStorageCheckerNilTypedClientSurvivesTrap is the explicit
// regression test for the typed-nil trap. A nil-valued *storage.Client
// variable (rather than the bare nil literal) is the exact shape
// production wiring produces when cfg.S3Endpoint is unset.
func TestNewStorageCheckerNilTypedClientSurvivesTrap(t *testing.T) {
	var client *storage.Client // typed nil
	c := NewStorageChecker(client)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("typed-nil client should short-circuit to OK, got %v", err)
	}
	if c.probe != nil {
		t.Fatalf("constructor failed to collapse typed-nil to nil interface: got %T(%v)", c.probe, c.probe)
	}
}

func TestStorageCheckerPropagatesProbeError(t *testing.T) {
	// Use the unexported field directly: tests live in the same
	// package and the storageProbe abstraction is internal.
	c := &StorageChecker{probe: &fakeStorageProbe{err: errors.New("403 forbidden")}}
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
	c := &StorageChecker{probe: &fakeStorageProbe{}}
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
