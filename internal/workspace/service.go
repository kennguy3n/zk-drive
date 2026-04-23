package workspace

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrForbidden is returned when the caller lacks permission to operate on a
// workspace.
var ErrForbidden = errors.New("forbidden")

// Service wraps the workspace repository with business-logic helpers used by
// the HTTP handlers.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create persists a workspace.
func (s *Service) Create(ctx context.Context, name string) (*Workspace, error) {
	w := &Workspace{Name: name}
	if err := s.repo.Create(ctx, w); err != nil {
		return nil, err
	}
	return w, nil
}

// GetByID returns a workspace by id.
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*Workspace, error) {
	return s.repo.GetByID(ctx, id)
}

// Update persists updates to the workspace metadata.
func (s *Service) Update(ctx context.Context, w *Workspace) error {
	return s.repo.Update(ctx, w)
}

// SetOwner records the initial owner_user_id for a workspace after the
// first admin has been created.
func (s *Service) SetOwner(ctx context.Context, workspaceID, ownerUserID uuid.UUID) error {
	return s.repo.SetOwner(ctx, workspaceID, ownerUserID)
}

// ListForUser returns every workspace the user belongs to.
func (s *Service) ListForUser(ctx context.Context, userID uuid.UUID) ([]*Workspace, error) {
	return s.repo.ListForUser(ctx, userID)
}
