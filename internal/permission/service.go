package permission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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

// Service wraps the permission repository with validation. For Phase 1 the
// service enforces only workspace-level grants on individual folders or
// files; folder permission inheritance is Phase 2.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
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
