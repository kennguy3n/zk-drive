package user

import (
	"context"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Service wraps the user repository with higher-level operations (password
// hashing, role assignment) used by the auth layer.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CreatePreservingHash persists a user without re-hashing the supplied
// password hash. Used when a user joins an additional workspace and we want
// to carry their existing credentials across rather than force a new
// password.
func (s *Service) CreatePreservingHash(ctx context.Context, u *User) error {
	return s.repo.Create(ctx, u)
}

// Create persists a user and hashes the supplied password with bcrypt.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, email, name, password, role string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := &User{
		WorkspaceID:  workspaceID,
		Email:        email,
		Name:         name,
		PasswordHash: string(hash),
		Role:         role,
	}
	if err := s.repo.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// VerifyPassword returns nil when the supplied password matches the stored
// hash for the user.
func (s *Service) VerifyPassword(u *User, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password))
}

// GetByID returns the user with the given id scoped to a workspace.
func (s *Service) GetByID(ctx context.Context, workspaceID, userID uuid.UUID) (*User, error) {
	return s.repo.GetByID(ctx, workspaceID, userID)
}

// GetByEmail returns the user with the given email within a workspace.
func (s *Service) GetByEmail(ctx context.Context, workspaceID uuid.UUID, email string) (*User, error) {
	return s.repo.GetByEmail(ctx, workspaceID, email)
}

// GetByEmailAnyWorkspace is a convenience for callers that only know the
// email (for example, login flows where the workspace_id is optional).
func (s *Service) GetByEmailAnyWorkspace(ctx context.Context, email string) (*User, error) {
	return s.repo.GetByEmailAnyWorkspace(ctx, email)
}

// List returns users belonging to a workspace.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID) ([]*User, error) {
	return s.repo.List(ctx, workspaceID)
}
