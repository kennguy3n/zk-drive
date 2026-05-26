package permission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ErrInvalidRole is returned when a caller supplies a role string outside
// the allowed set.
var ErrInvalidRole = errors.New("invalid role")

// ErrInvalidResourceType is returned when a caller supplies a resource_type
// outside the allowed set.
var ErrInvalidResourceType = errors.New("invalid resource_type")

// ErrInvalidGranteeType is returned when a caller supplies a grantee_type
// outside the allowed set.
var ErrInvalidGranteeType = errors.New("invalid grantee_type")

// Service wraps the permission repository with validation. It
// enforces only workspace-level grants on individual folders or
// files today; folder permission inheritance is a follow-up.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// WithCache wraps the underlying Repository with a Redis-backed
// read-through CachedRepository. Returns the same *Service for
// fluent chaining at wiring time. Idempotent: calling twice
// replaces the previous cache layer (the second call's rdb / ttl
// / obs win).
//
// rdb may be nil — when REDIS_URL is unset the wiring code calls
// this with a nil client and we silently leave the un-cached
// repository in place. The decision to skip caching when Redis
// is unavailable lives at the wiring site (cmd/server/main.go);
// the service layer is the no-op pass-through.
//
// ttl is the per-entry expiry written on every cache fill.
// Callers should source it from config.Config.PerformanceCacheTTL
// so the clamp is applied centrally.
//
// obs may be nil; the cache layer substitutes a no-op observer
// internally.
//
// Returns the same *Service for chaining.
func (s *Service) WithCache(rdb redis.UniversalClient, ttl time.Duration, obs CacheObserver) *Service {
	if rdb == nil || ttl <= 0 {
		return s
	}
	// If the repository is already a *CachedRepository we
	// rewrap its underlying delegate so a second WithCache
	// call doesn't double-decorate.
	delegate := s.repo
	if existing, ok := delegate.(*CachedRepository); ok {
		delegate = existing.Underlying()
	}
	s.repo = NewCachedRepository(delegate, rdb, ttl, obs)
	return s
}

// BustWorkspace invalidates every cached access-check result for
// the workspace when the underlying repository is a
// CachedRepository. No-op otherwise. Exposed so service callers
// in other packages (folder.Service.Move, file.Service.Move) can
// invalidate the cache when an ancestry-mutating event happens
// outside the permission service itself.
//
// The bust is best-effort and never returns an error: cache
// invalidation failures are a perf concern, not a correctness
// concern (entries self-expire via TTL), so we explicitly do not
// surface them to callers — every Move / Delete call site
// already has its own error path that must not be polluted by
// cache plumbing.
func (s *Service) BustWorkspace(ctx context.Context, workspaceID uuid.UUID) {
	if c, ok := s.repo.(*CachedRepository); ok {
		c.BustWorkspace(ctx, workspaceID)
	}
}

// Grant creates a new permission grant on a resource. All arguments are
// validated against the allowed-values sets so callers cannot smuggle an
// unknown role or resource type into the database (the migration's CHECK
// constraints back this up, but we fail fast at the service layer to keep
// error messages informative).
func (s *Service) Grant(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, role string, expiresAt *time.Time) (*Permission, error) {
	if !isValidResourceType(resourceType) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidResourceType, resourceType)
	}
	if !isValidGranteeType(granteeType) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidGranteeType, granteeType)
	}
	if !isValidRole(role) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	p := &Permission{
		WorkspaceID:  workspaceID,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		GranteeType:  granteeType,
		GranteeID:    granteeID,
		Role:         role,
		ExpiresAt:    expiresAt,
	}
	if err := s.repo.Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Revoke deletes a permission by id.
func (s *Service) Revoke(ctx context.Context, workspaceID, permID uuid.UUID) error {
	return s.repo.Delete(ctx, workspaceID, permID)
}

// HasAccess reports whether the grantee has at least minRole on the
// resource within the workspace.
func (s *Service) HasAccess(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	if !isValidResourceType(resourceType) {
		return false, fmt.Errorf("%w: %q", ErrInvalidResourceType, resourceType)
	}
	if !isValidGranteeType(granteeType) {
		return false, fmt.Errorf("%w: %q", ErrInvalidGranteeType, granteeType)
	}
	if !isValidRole(minRole) {
		return false, fmt.Errorf("%w: %q", ErrInvalidRole, minRole)
	}
	return s.repo.CheckAccess(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, minRole)
}

// HasAccessWithInheritance reports whether the grantee has at least
// minRole on the resource, considering grants inherited from ancestor
// folders. Resolution semantics are documented on
// PostgresRepository.CheckAccessWithInheritance; the "most-specific
// wins" rule (ARCHITECTURE.md §7.2) means an explicit grant on a child
// can override inherited grants from parents. HasAccess remains the
// flat, non-inheriting check for callers that know they do not want to
// walk the folder tree.
func (s *Service) HasAccessWithInheritance(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	if !isValidResourceType(resourceType) {
		return false, fmt.Errorf("%w: %q", ErrInvalidResourceType, resourceType)
	}
	if !isValidGranteeType(granteeType) {
		return false, fmt.Errorf("%w: %q", ErrInvalidGranteeType, granteeType)
	}
	if !isValidRole(minRole) {
		return false, fmt.Errorf("%w: %q", ErrInvalidRole, minRole)
	}
	return s.repo.CheckAccessWithInheritance(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, minRole)
}

// ListForResource returns every grant on a given resource.
func (s *Service) ListForResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]*Permission, error) {
	if !isValidResourceType(resourceType) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidResourceType, resourceType)
	}
	return s.repo.ListByResource(ctx, workspaceID, resourceType, resourceID)
}

// GetByID fetches a permission by id, scoped to a workspace.
func (s *Service) GetByID(ctx context.Context, workspaceID, permID uuid.UUID) (*Permission, error) {
	return s.repo.GetByID(ctx, workspaceID, permID)
}
