package feature

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/billing"
)

// TierResolver resolves the billing tier for a workspace. It is the only
// dependency the feature service has on billing, declared as a narrow
// interface so the service is trivially testable with a fake and never
// imports the concrete billing service type.
type TierResolver interface {
	Tier(ctx context.Context, workspaceID uuid.UUID) (string, error)
}

// Service computes the effective feature set for a workspace by layering
// per-workspace overrides (from the repository) on top of tier defaults
// (from flags.go). It is the single source of truth consulted by both the
// GET /api/features handler and any server-side feature gate.
//
// A nil *Service is a valid receiver: every feature resolves to its
// Free-tier default with no overrides. This mirrors the nil-safe
// convention used by billing.Service and lets test wiring omit the
// service without panicking.
type Service struct {
	repo  Repository
	tiers TierResolver
}

// NewService builds a Service over repo and tier resolver. Either may be
// nil: a nil repo means "no overrides" (tier defaults only); a nil tier
// resolver means "every workspace resolves to Free".
func NewService(repo Repository, tiers TierResolver) *Service {
	return &Service{repo: repo, tiers: tiers}
}

// resolveTier returns the workspace's billing tier, defaulting to Free on
// a nil service / resolver or on lookup error. Feature gating must never
// hard-fail a request: an unresolved tier degrades to the most
// restrictive (Free) set rather than erroring, so a transient billing
// hiccup hides paid UI rather than breaking the app or, worse, unlocking
// features.
func (s *Service) resolveTier(ctx context.Context, workspaceID uuid.UUID) string {
	if s == nil || s.tiers == nil {
		return billing.TierFree
	}
	tier, err := s.tiers.Tier(ctx, workspaceID)
	if err != nil || tier == "" {
		return billing.TierFree
	}
	return tier
}

// ActiveFeatures returns the effective enabled/disabled state of every
// known feature for a workspace: tier defaults with per-workspace
// overrides applied on top. The returned map always contains every key in
// AllFeatures so the caller sees a complete, fixed schema.
//
// It also returns the resolved tier so the handler can surface it to the
// client (the UI shows the current plan and gates "upgrade" prompts).
func (s *Service) ActiveFeatures(ctx context.Context, workspaceID uuid.UUID) (map[string]bool, string, error) {
	tier := s.resolveTier(ctx, workspaceID)
	features := DefaultsForTier(tier)

	if s != nil && s.repo != nil {
		overrides, err := s.repo.GetOverrides(ctx, workspaceID)
		if err != nil {
			return nil, "", fmt.Errorf("feature: load overrides: %w", err)
		}
		for key, enabled := range overrides {
			// GetOverrides already filters unknown keys, but guard here
			// too so a future repo can't widen the schema by accident.
			if _, ok := features[key]; ok {
				features[key] = enabled
			}
		}
	}
	return features, tier, nil
}

// IsEnabled reports whether a single feature is active for a workspace.
// Unknown features resolve to false (fail-closed): a server-side gate that
// can't confirm a feature is on must treat it as off. Use ActiveFeatures
// when you need several flags at once to avoid repeated tier + override
// lookups.
//
// If the per-workspace override lookup fails we fall back to the resolved
// tier default rather than false. The override is only a delta on top of
// the tier default, so when it can't be read the tier baseline is the
// correct answer: a transient workspace_features outage must not disable
// baseline features (folders, files, …) for an entitled workspace, and the
// fallback can never grant a feature above the tier because DefaultEnabled
// already encodes the tier ceiling. The only state lost in that window is a
// kill-switch override, which is an accepted trade-off for keeping core
// functionality available.
func (s *Service) IsEnabled(ctx context.Context, workspaceID uuid.UUID, feature string) bool {
	if !IsKnownFeature(feature) {
		return false
	}
	tier := s.resolveTier(ctx, workspaceID)
	enabled := DefaultEnabled(tier, feature)

	if s != nil && s.repo != nil {
		overrides, err := s.repo.GetOverrides(ctx, workspaceID)
		if err != nil {
			return enabled
		}
		if v, ok := overrides[feature]; ok {
			enabled = v
		}
	}
	return enabled
}

// SetOverride explicitly enables or disables a feature for a workspace,
// overriding its tier default. feature must be a known key. updatedBy is
// the admin user making the change (nil for system-initiated changes).
func (s *Service) SetOverride(ctx context.Context, workspaceID uuid.UUID, feature string, enabled bool, updatedBy *uuid.UUID) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("feature: service not configured")
	}
	if !IsKnownFeature(feature) {
		return fmt.Errorf("feature: unknown feature %q", feature)
	}
	return s.repo.SetOverride(ctx, workspaceID, feature, enabled, updatedBy)
}

// ClearOverride removes a feature override, reverting the feature to its
// tier default.
func (s *Service) ClearOverride(ctx context.Context, workspaceID uuid.UUID, feature string) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("feature: service not configured")
	}
	if !IsKnownFeature(feature) {
		return fmt.Errorf("feature: unknown feature %q", feature)
	}
	return s.repo.DeleteOverride(ctx, workspaceID, feature)
}

// billingTierResolver adapts *billing.Service to the TierResolver
// interface. billing.Service.LimitsFor already resolves a workspace's
// tier (falling back to Free when no plan row exists), so we read the
// resolved Limits.Tier rather than reaching into the repository.
type billingTierResolver struct {
	billing *billing.Service
}

// NewBillingTierResolver returns a TierResolver backed by the billing
// service. A nil billing service resolves every workspace to Free.
func NewBillingTierResolver(b *billing.Service) TierResolver {
	return &billingTierResolver{billing: b}
}

func (r *billingTierResolver) Tier(ctx context.Context, workspaceID uuid.UUID) (string, error) {
	if r == nil || r.billing == nil {
		return billing.TierFree, nil
	}
	limits, _, err := r.billing.LimitsFor(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	return limits.Tier, nil
}
