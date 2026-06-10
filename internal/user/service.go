package user

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	appcrypto "github.com/kennguy3n/zk-drive/internal/crypto"
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

// CreatePreservingHashTx is the tx-aware equivalent of CreatePreservingHash.
func (s *Service) CreatePreservingHashTx(ctx context.Context, tx pgx.Tx, u *User) error {
	return s.repo.CreateTx(ctx, tx, u)
}

// Create persists a user and hashes the supplied password with bcrypt.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, email, name, password, role string) (*User, error) {
	u, err := buildUser(workspaceID, email, name, password, role)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// CreateTx is the tx-aware equivalent of Create, used by signup-style flows
// that need to create a workspace and its first admin user atomically.
func (s *Service) CreateTx(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID, email, name, password, role string) (*User, error) {
	u, err := buildUser(workspaceID, email, name, password, role)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateTx(ctx, tx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// CreateFederated persists a user authenticated by an external identity
// provider (iam-core). No password is hashed; the row is stamped with
// (provider, providerID) so subsequent logins resolve by the upstream
// subject id via GetByAuthProvider, and a non-bcrypt password sentinel
// is stored so the local password-login path can never match. The
// returned User has its ID and timestamps populated.
func (s *Service) CreateFederated(ctx context.Context, workspaceID uuid.UUID, email, name, role, provider, providerID string) (*User, error) {
	if provider == "" || providerID == "" {
		return nil, fmt.Errorf("user: federated create requires provider and provider id")
	}
	u := &User{
		WorkspaceID:    workspaceID,
		Email:          email,
		Name:           name,
		PasswordHash:   FederatedPasswordSentinel,
		Role:           role,
		AuthProvider:   &provider,
		AuthProviderID: &providerID,
	}
	if err := s.repo.CreateFederated(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func buildUser(workspaceID uuid.UUID, email, name, password, role string) (*User, error) {
	hash, err := appcrypto.HashPassword(password)
	if err != nil {
		return nil, err
	}
	return &User{
		WorkspaceID:  workspaceID,
		Email:        email,
		Name:         name,
		PasswordHash: string(hash),
		Role:         role,
	}, nil
}

// VerifyPassword returns nil when the supplied password matches the stored
// hash for the user.
func (s *Service) VerifyPassword(u *User, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password))
}

// MaybeRehashPassword upgrades the user's stored bcrypt hash to the
// current appcrypto.PasswordHashCost if the stored hash was created
// at a lower cost. Call this AFTER a successful VerifyPassword — it
// re-hashes the plaintext the caller already verified.
//
// This is the rehash-on-login pattern: bumping the cost constant
// only protects new users until existing users' hashes are upgraded.
// Without this, a cost bump from 10 to 12 leaves every pre-existing
// password vulnerable at the old factor until the user manually
// changes their password.
//
// The function is best-effort: any failure (cost-parse error, DB
// write error, in-flight schema change) is returned to the caller
// so it can be logged, but callers should NOT propagate this error
// to the client — the user's login already succeeded and a failed
// rehash is a silent operational issue, not an auth failure. The
// in-memory u.PasswordHash is also updated on success so subsequent
// VerifyPassword calls in the same request use the new hash.
func (s *Service) MaybeRehashPassword(ctx context.Context, u *User, password string) error {
	storedCost, err := bcrypt.Cost([]byte(u.PasswordHash))
	if err != nil {
		return fmt.Errorf("inspect stored hash cost: %w", err)
	}
	if storedCost >= appcrypto.PasswordHashCost {
		return nil
	}
	newHash, err := appcrypto.HashPassword(password)
	if err != nil {
		return fmt.Errorf("rehash password: %w", err)
	}
	if err := s.repo.UpdatePasswordHash(ctx, u.ID, string(newHash)); err != nil {
		return fmt.Errorf("persist rehashed password: %w", err)
	}
	u.PasswordHash = string(newHash)
	return nil
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

// GetByAuthProvider returns the user matched on (auth_provider,
// auth_provider_id) across all workspaces. Used by the built-in OAuth
// callback. Not workspace-scoped — see the repository docstring.
func (s *Service) GetByAuthProvider(ctx context.Context, provider, providerID string) (*User, error) {
	return s.repo.GetByAuthProvider(ctx, provider, providerID)
}

// GetByWorkspaceAndAuthProvider returns the federated user row for a
// subject within a specific workspace. Used by the iam-core middleware,
// which resolves the authoritative workspace from the token's tenant/org
// claims before looking up the user.
func (s *Service) GetByWorkspaceAndAuthProvider(ctx context.Context, workspaceID uuid.UUID, provider, providerID string) (*User, error) {
	return s.repo.GetByWorkspaceAndAuthProvider(ctx, workspaceID, provider, providerID)
}

// UpdateLastLogin stamps the user's last_login_at column. Call after a
// successful authentication (password or SSO).
func (s *Service) UpdateLastLogin(ctx context.Context, userID uuid.UUID, at time.Time) error {
	return s.repo.UpdateLastLogin(ctx, userID, at)
}

// Deactivate soft-deactivates a user. The row is preserved so audit
// history still resolves the actor.
func (s *Service) Deactivate(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time) error {
	return s.repo.Deactivate(ctx, workspaceID, userID, at)
}

// UpdateRole changes a user's role within a workspace.
func (s *Service) UpdateRole(ctx context.Context, workspaceID, userID uuid.UUID, role string) error {
	return s.repo.UpdateRole(ctx, workspaceID, userID, role)
}

// LinkAuthProvider stamps an existing user with a (provider,
// provider_id) pair so subsequent SSO logins resolve by subject id.
func (s *Service) LinkAuthProvider(ctx context.Context, userID uuid.UUID, provider, providerID string) error {
	return s.repo.LinkAuthProvider(ctx, userID, provider, providerID)
}
