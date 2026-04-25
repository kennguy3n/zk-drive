package billing

import (
	"context"
	"errors"
	"log"

	"github.com/google/uuid"
)

// Service composes the Repository with quota-enforcement helpers.
// All methods are safe to call on a nil receiver — quota checks
// short-circuit to "allow" and recordEvent calls become no-ops so
// the rest of the system keeps working when billing is intentionally
// disabled (e.g. in unit-test wiring).
type Service struct {
	repo Repository
}

// NewService wraps r in a Service.
func NewService(r Repository) *Service {
	return &Service{repo: r}
}

// LimitsFor returns the resolved limits for a workspace, falling
// back to the free-tier defaults when no plan row exists.
func (s *Service) LimitsFor(ctx context.Context, workspaceID uuid.UUID) (Limits, *Plan, error) {
	if s == nil {
		return DefaultLimitsFor(TierFree), nil, nil
	}
	p, err := s.repo.GetPlan(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, ErrPlanNotFound) {
			return DefaultLimitsFor(TierFree), nil, nil
		}
		return Limits{}, nil, err
	}
	return p.EffectiveLimits(), p, nil
}

// UpsertPlan changes or creates a workspace's plan. Returns the
// resulting row.
func (s *Service) UpsertPlan(ctx context.Context, p *Plan) (*Plan, error) {
	if s == nil {
		return nil, errors.New("billing service not configured")
	}
	if !IsValidTier(p.Tier) {
		return nil, errors.New("billing: invalid tier")
	}
	return s.repo.UpsertPlan(ctx, p)
}

// CheckStorageQuota returns ErrQuotaExceeded when adding
// additionalBytes to the workspace's current storage would push it
// past the plan's MaxStorageBytes.
func (s *Service) CheckStorageQuota(ctx context.Context, workspaceID uuid.UUID, additionalBytes int64) error {
	if s == nil {
		return nil
	}
	limits, _, err := s.LimitsFor(ctx, workspaceID)
	if err != nil {
		return err
	}
	if limits.MaxStorageBytes <= 0 {
		return nil
	}
	used, err := s.repo.GetStorageUsed(ctx, workspaceID)
	if err != nil {
		return err
	}
	if used+additionalBytes > limits.MaxStorageBytes {
		return ErrQuotaExceeded
	}
	return nil
}

// CheckUserQuota returns ErrQuotaExceeded when the workspace already
// holds the seat limit. Called before InviteUser.
func (s *Service) CheckUserQuota(ctx context.Context, workspaceID uuid.UUID) error {
	if s == nil {
		return nil
	}
	limits, _, err := s.LimitsFor(ctx, workspaceID)
	if err != nil {
		return err
	}
	if limits.MaxUsers <= 0 {
		return nil
	}
	n, err := s.repo.GetUserCount(ctx, workspaceID)
	if err != nil {
		return err
	}
	if n+1 > limits.MaxUsers {
		return ErrQuotaExceeded
	}
	return nil
}

// CheckBandwidthQuota returns ErrQuotaExceeded when bytes worth of
// download would push the month-to-date total past the limit.
func (s *Service) CheckBandwidthQuota(ctx context.Context, workspaceID uuid.UUID, bytes int64) error {
	if s == nil {
		return nil
	}
	limits, _, err := s.LimitsFor(ctx, workspaceID)
	if err != nil {
		return err
	}
	if limits.MaxBandwidthBytesMonthly <= 0 {
		return nil
	}
	used, err := s.repo.GetBandwidthUsedThisMonth(ctx, workspaceID)
	if err != nil {
		return err
	}
	if used+bytes > limits.MaxBandwidthBytesMonthly {
		return ErrQuotaExceeded
	}
	return nil
}

// RecordUpload appends a storage event. Used after a confirm-upload
// completes; the storage *truth* still comes from the files table,
// but the event captures upload activity for charts and audits.
func (s *Service) RecordUpload(ctx context.Context, workspaceID uuid.UUID, bytes int64) {
	s.recordEvent(ctx, workspaceID, EventStorage, bytes)
}

// RecordDownload appends a bandwidth event.
func (s *Service) RecordDownload(ctx context.Context, workspaceID uuid.UUID, bytes int64) {
	s.recordEvent(ctx, workspaceID, EventBandwidth, bytes)
}

// RecordUserAdded appends a user_added event.
func (s *Service) RecordUserAdded(ctx context.Context, workspaceID uuid.UUID) {
	s.recordEvent(ctx, workspaceID, EventUserAdded, 0)
}

func (s *Service) recordEvent(ctx context.Context, workspaceID uuid.UUID, t string, bytes int64) {
	if s == nil {
		return
	}
	if err := s.repo.RecordEvent(ctx, workspaceID, t, bytes); err != nil {
		// Recording is best-effort: a usage write failure must not
		// fail an otherwise-successful upload / download. Log so an
		// operator can spot a broken billing pipeline.
		log.Printf("billing: record %s event for workspace %s: %v", t, workspaceID, err)
	}
}

// UsageSummary is the response shape for GET /api/admin/billing/usage.
// Embeds the limits + the live counters so the frontend can render
// progress bars without a second round-trip.
type UsageSummary struct {
	Tier            string `json:"tier"`
	StorageUsed     int64  `json:"storage_used_bytes"`
	StorageLimit    int64  `json:"storage_limit_bytes"`
	BandwidthUsed   int64  `json:"bandwidth_used_bytes_month"`
	BandwidthLimit  int64  `json:"bandwidth_limit_bytes_month"`
	UserCount       int    `json:"user_count"`
	UserLimit       int    `json:"user_limit"`
	PlanConfigured  bool   `json:"plan_configured"`
}

// GetUsageSummary aggregates current usage and the effective limits
// for a workspace.
func (s *Service) GetUsageSummary(ctx context.Context, workspaceID uuid.UUID) (UsageSummary, error) {
	limits, plan, err := s.LimitsFor(ctx, workspaceID)
	if err != nil {
		return UsageSummary{}, err
	}
	storage, err := s.repo.GetStorageUsed(ctx, workspaceID)
	if err != nil {
		return UsageSummary{}, err
	}
	bandwidth, err := s.repo.GetBandwidthUsedThisMonth(ctx, workspaceID)
	if err != nil {
		return UsageSummary{}, err
	}
	users, err := s.repo.GetUserCount(ctx, workspaceID)
	if err != nil {
		return UsageSummary{}, err
	}
	return UsageSummary{
		Tier:            limits.Tier,
		StorageUsed:     storage,
		StorageLimit:    limits.MaxStorageBytes,
		BandwidthUsed:   bandwidth,
		BandwidthLimit:  limits.MaxBandwidthBytesMonthly,
		UserCount:       users,
		UserLimit:       limits.MaxUsers,
		PlanConfigured:  plan != nil,
	}, nil
}
