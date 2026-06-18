// Package setup backs the guided first-boot setup wizard.
//
// A fresh SME deployment has an empty database and an operator with no
// ops knowledge. The frontend walks them through a five-step wizard
// (admin account → storage → optional services → first workspace →
// first invite). This package answers two questions for that flow:
//
//   - GET /api/setup/status — what is already configured and what is
//     still missing, so the UI knows whether to show the wizard and
//     which step to resume on.
//   - POST /api/setup/complete — record that the operator finished (or
//     deliberately dismissed) the wizard, in the setup_state singleton
//     from migration 041, so it stays dismissed.
//
// "Configured" for the infrastructure-backed pieces (storage, email,
// virus scanning, AI, collaborative editing) is a pure function of the
// process configuration, which cannot change without a restart, so
// those booleans are captured once at construction. The dynamic pieces
// (does an admin account exist? does a workspace exist? is the flag
// set?) are read live from the database on each call.
package setup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Capabilities captures the infrastructure-backed configuration the
// wizard reports on. It is derived from the process config at startup
// (see FromConfig in cmd/server) and is immutable for the life of the
// process.
type Capabilities struct {
	// StorageConfigured is true when the full S3/Fabric credential
	// set (endpoint, bucket, access key, secret key) is present, so
	// uploads can actually be stored. This is the one required
	// service; the rest are optional.
	StorageConfigured bool
	// Optional services. Each is independently toggleable and the
	// product runs without any of them (degrading the relevant
	// feature) — the wizard surfaces them as opt-in switches.
	EmailConfigured                bool
	VirusScanningConfigured        bool
	AIConfigured                   bool
	CollaborativeEditingConfigured bool
}

// Service answers setup-status questions and records completion.
type Service struct {
	pool *pgxpool.Pool
	caps Capabilities
}

// NewService constructs a setup Service over the primary pool. caps is
// the immutable, config-derived capability snapshot.
func NewService(pool *pgxpool.Pool, caps Capabilities) *Service {
	return &Service{pool: pool, caps: caps}
}

// Step reports whether a single wizard step's prerequisite is
// satisfied, with an optional short human-readable detail. The detail
// is intentionally non-sensitive (never echoes credentials) because
// the status endpoint is reachable pre-authentication on a fresh box.
type Step struct {
	Configured bool   `json:"configured"`
	Detail     string `json:"detail,omitempty"`
}

// OptionalServices reports which opt-in integrations are wired.
type OptionalServices struct {
	Email                bool `json:"email"`
	VirusScanning        bool `json:"virus_scanning"`
	AI                   bool `json:"ai"`
	CollaborativeEditing bool `json:"collaborative_editing"`
}

// Steps mirrors the wizard's required steps so the UI can render a
// resumable checklist. Optional services are reported separately
// because they never block completion.
type Steps struct {
	AdminAccount     Step             `json:"admin_account"`
	Storage          Step             `json:"storage"`
	Workspace        Step             `json:"workspace"`
	OptionalServices OptionalServices `json:"optional_services"`
}

// Status is the GET /api/setup/status response.
//
// Detail (Steps) is only populated while setup is incomplete: a fresh
// box has no secrets worth protecting and the unauthenticated wizard
// needs the breakdown to drive itself. Once setup is complete the
// endpoint returns the two booleans only, so an already-provisioned
// (and potentially internet-exposed) install does not leak its
// deployment shape to anonymous callers.
type Status struct {
	SetupCompleted bool       `json:"setup_completed"`
	NeedsSetup     bool       `json:"needs_setup"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Steps          *Steps     `json:"steps,omitempty"`
}

// Status reads the live setup state and composes the response.
func (s *Service) Status(ctx context.Context) (Status, error) {
	completed, completedAt, err := s.completion(ctx)
	if err != nil {
		return Status{}, err
	}

	if completed {
		// Minimal, non-revealing response for a provisioned install.
		st := Status{SetupCompleted: true, NeedsSetup: false}
		if !completedAt.IsZero() {
			ca := completedAt
			st.CompletedAt = &ca
		}
		return st, nil
	}

	hasAdmin, err := s.hasAdminAccount(ctx)
	if err != nil {
		return Status{}, err
	}
	workspaceCount, err := s.workspaceCount(ctx)
	if err != nil {
		return Status{}, err
	}

	steps := &Steps{
		AdminAccount: Step{Configured: hasAdmin},
		Storage:      Step{Configured: s.caps.StorageConfigured},
		Workspace:    Step{Configured: workspaceCount > 0},
		OptionalServices: OptionalServices{
			Email:                s.caps.EmailConfigured,
			VirusScanning:        s.caps.VirusScanningConfigured,
			AI:                   s.caps.AIConfigured,
			CollaborativeEditing: s.caps.CollaborativeEditingConfigured,
		},
	}
	if !s.caps.StorageConfigured {
		steps.Storage.Detail = "S3 endpoint, bucket, and access keys are not all set"
	}

	// needs_setup is the single signal the frontend gates the wizard
	// on. A box "needs setup" until it has at least one workspace —
	// the minimum required to actually use the product — and the flag
	// has not been explicitly set. We intentionally do not require
	// every optional service, since those are opt-in.
	return Status{
		SetupCompleted: false,
		NeedsSetup:     workspaceCount == 0,
		Steps:          steps,
	}, nil
}

// IsCompleted reports just the durable flag, for callers that only
// need the boolean (e.g. gating the unauthenticated test-storage
// endpoint).
func (s *Service) IsCompleted(ctx context.Context) (bool, error) {
	completed, _, err := s.completion(ctx)
	return completed, err
}

// MarkCompleted flips the singleton flag to true and stamps the
// completion time. Idempotent: re-marking an already-complete install
// leaves the original completed_at intact so the audit trail of "when
// did this install first finish setup" is preserved.
func (s *Service) MarkCompleted(ctx context.Context) error {
	const q = `
UPDATE setup_state
SET completed = TRUE,
    completed_at = COALESCE(completed_at, now())
WHERE id = TRUE`
	if _, err := s.pool.Exec(ctx, q); err != nil {
		return fmt.Errorf("setup: mark completed: %w", err)
	}
	return nil
}

// completion reads the singleton row. The seed row from migration 041
// guarantees exactly one row exists, but we tolerate its absence
// (treating it as "not completed") so the endpoint degrades gracefully
// rather than erroring if the migration has not run yet.
func (s *Service) completion(ctx context.Context) (bool, time.Time, error) {
	const q = `SELECT completed, COALESCE(completed_at, 'epoch'::timestamptz) FROM setup_state WHERE id = TRUE`
	var completed bool
	var at time.Time
	err := s.pool.QueryRow(ctx, q).Scan(&completed, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("setup: read state: %w", err)
	}
	if at.Equal(time.Unix(0, 0).UTC()) {
		at = time.Time{}
	}
	return completed, at, nil
}

// hasAdminAccount reports whether at least one admin user exists. The
// wizard's step 1 creates the first admin; until then the install has
// nobody who can authenticate to finish setup.
func (s *Service) hasAdminAccount(ctx context.Context) (bool, error) {
	const q = `SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`
	var exists bool
	if err := s.pool.QueryRow(ctx, q).Scan(&exists); err != nil {
		return false, fmt.Errorf("setup: count admins: %w", err)
	}
	return exists, nil
}

// workspaceCount returns the number of workspaces. Zero means a truly
// fresh install that still needs the wizard.
func (s *Service) workspaceCount(ctx context.Context) (int, error) {
	const q = `SELECT count(*) FROM workspaces`
	var n int
	if err := s.pool.QueryRow(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("setup: count workspaces: %w", err)
	}
	return n, nil
}
