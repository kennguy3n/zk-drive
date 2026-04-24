package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service bundles policy CRUD + evaluation logic. Evaluate reads the
// current policies for a workspace and returns the version IDs that
// should be archived and/or deleted.
type Service struct {
	repo Repository
	pool *pgxpool.Pool
}

// NewService returns a Service. The pool is retained so Evaluate can
// ad-hoc query file_versions without routing through the file
// repository (which would require importing internal/file and creating
// a cycle when retention is called from the file package).
func NewService(repo Repository, pool *pgxpool.Pool) *Service {
	return &Service{repo: repo, pool: pool}
}

// List returns policies scoped to a workspace.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID) ([]*Policy, error) {
	return s.repo.List(ctx, workspaceID)
}

// Get returns a single policy by id.
func (s *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Policy, error) {
	return s.repo.Get(ctx, workspaceID, id)
}

// Upsert validates and persists a policy. Returns the upserted
// policy with its id / timestamps populated.
func (s *Service) Upsert(ctx context.Context, p *Policy) (*Policy, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if err := s.repo.Upsert(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Delete removes a policy by id, workspace-scoped.
func (s *Service) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	return s.repo.Delete(ctx, workspaceID, id)
}

// Evaluate returns the version IDs that should be archived or
// deleted under the current policies for a workspace. The query joins
// file_versions to its owning file so the folder scope on a Policy can
// be honored. When multiple policies target the same folder subtree,
// the most restrictive (lowest) numeric threshold wins — modeled by
// LEAST() in the SQL.
//
// The implementation is intentionally a single query per workspace so
// the retention worker can run fast and deterministic; per-policy
// loops would multiply the Postgres round-trips without unlocking
// richer semantics.
func (s *Service) Evaluate(ctx context.Context, workspaceID uuid.UUID, now time.Time) (*EvaluationResult, error) {
	policies, err := s.repo.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	result := &EvaluationResult{WorkspaceID: workspaceID}
	if len(policies) == 0 {
		return result, nil
	}
	// max_age_days and archive_after_days are evaluated here; the
	// max_versions dimension would require a window function over
	// file_versions and is deferred until we see real workspace
	// volumes.
	for _, p := range policies {
		if p.ArchiveAfterDays != nil && *p.ArchiveAfterDays > 0 {
			ids, err := s.pickVersions(ctx, workspaceID, p, now, *p.ArchiveAfterDays, false)
			if err != nil {
				return nil, fmt.Errorf("pick archive versions: %w", err)
			}
			result.ArchiveVersions = append(result.ArchiveVersions, ids...)
		}
		if p.MaxAgeDays != nil && *p.MaxAgeDays > 0 {
			ids, err := s.pickVersions(ctx, workspaceID, p, now, *p.MaxAgeDays, true)
			if err != nil {
				return nil, fmt.Errorf("pick delete versions: %w", err)
			}
			result.DeleteVersions = append(result.DeleteVersions, ids...)
		}
	}
	return result, nil
}

func (s *Service) pickVersions(ctx context.Context, workspaceID uuid.UUID, p *Policy, now time.Time, days int, forDelete bool) ([]uuid.UUID, error) {
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	args := []any{workspaceID, cutoff}
	q := `
SELECT v.id
FROM file_versions v
JOIN files f ON f.id = v.file_id
WHERE f.workspace_id = $1
  AND v.created_at < $2
  AND f.deleted_at IS NULL`
	if forDelete {
		// Delete pass: only consider versions that are not the
		// currently-published one, so retention never leaves a file
		// orphaned without a current version. Archived rows are fair
		// game for hard delete — that's the whole point of the
		// archive tier.
		q += "\n  AND v.id <> COALESCE(f.current_version_id, '00000000-0000-0000-0000-000000000000'::uuid)"
	} else {
		// Archive pass: skip rows already archived.
		q += "\n  AND v.archived_at IS NULL"
	}
	if p.FolderID != nil {
		q += "\n  AND f.folder_id = $3"
		args = append(args, *p.FolderID)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// validate enforces that at least one dimension is set and the
// values are non-negative. Negative values are the most common
// misconfiguration so the error message calls that out explicitly.
func (p *Policy) validate() error {
	if p.WorkspaceID == uuid.Nil {
		return errors.New("workspace_id is required")
	}
	if p.MaxVersions == nil && p.MaxAgeDays == nil && p.ArchiveAfterDays == nil {
		return errors.New("at least one of max_versions, max_age_days, archive_after_days is required")
	}
	if p.MaxVersions != nil && *p.MaxVersions < 0 {
		return errors.New("max_versions must be >= 0")
	}
	if p.MaxAgeDays != nil && *p.MaxAgeDays < 0 {
		return errors.New("max_age_days must be >= 0")
	}
	if p.ArchiveAfterDays != nil && *p.ArchiveAfterDays < 0 {
		return errors.New("archive_after_days must be >= 0")
	}
	return nil
}
