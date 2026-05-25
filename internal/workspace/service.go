package workspace

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// CreateTx is the tx-aware equivalent of Create, used by signup-style flows
// that need to create a workspace and its first admin user atomically.
func (s *Service) CreateTx(ctx context.Context, tx pgx.Tx, name string) (*Workspace, error) {
	w := &Workspace{Name: name}
	if err := s.repo.CreateTx(ctx, tx, w); err != nil {
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

// SetOwnerTx is the tx-aware equivalent of SetOwner.
func (s *Service) SetOwnerTx(ctx context.Context, tx pgx.Tx, workspaceID, ownerUserID uuid.UUID) error {
	return s.repo.SetOwnerTx(ctx, tx, workspaceID, ownerUserID)
}

// ListForUser returns every workspace the user belongs to.
func (s *Service) ListForUser(ctx context.Context, userID uuid.UUID) ([]*Workspace, error) {
	return s.repo.ListForUser(ctx, userID)
}

// SetMFARequired flips the workspaces.mfa_required policy and
// returns the previous value so the caller can record the
// transition in the audit log. See PostgresRepository.SetMFARequired
// for the locking semantics.
func (s *Service) SetMFARequired(ctx context.Context, workspaceID uuid.UUID, required bool) (bool, error) {
	return s.repo.SetMFARequired(ctx, workspaceID, required)
}

// ErrUnsupportedSearchLanguage is returned when the caller asks for
// a Postgres dictionary that isn't on the IsSupportedSearchLanguage
// allow-list. The handler maps it to 400 with the supported set in
// the response body.
var ErrUnsupportedSearchLanguage = errors.New("workspace: unsupported search language")

// SetSearchLanguage updates the workspaces.search_language column
// after validating lang against the IsSupportedSearchLanguage
// allow-list. Returns the previous value (for the audit log) and
// ErrUnsupportedSearchLanguage when lang is not on the allow-list.
func (s *Service) SetSearchLanguage(ctx context.Context, workspaceID uuid.UUID, lang string) (string, error) {
	if !IsSupportedSearchLanguage(lang) {
		return "", ErrUnsupportedSearchLanguage
	}
	return s.repo.SetSearchLanguage(ctx, workspaceID, lang)
}

// GetSearchLanguage returns the workspace's currently-configured
// FTS dictionary. Centralised here so callers (search handler,
// admin GET endpoint) don't reach through to the repository.
// Falls back to DefaultSearchLanguage when the workspace exists
// but the column is somehow empty — defence in depth against a
// future migration that forgets to set NOT NULL.
func (s *Service) GetSearchLanguage(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	w, err := s.repo.GetByID(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if w.SearchLanguage == "" {
		return DefaultSearchLanguage, nil
	}
	return w.SearchLanguage, nil
}
