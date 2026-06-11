package feature

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/billing"
)

// fakeRepo is an in-memory Repository for unit tests.
type fakeRepo struct {
	overrides map[string]bool
	getErr    error
	setCalls  int
	delCalls  int
}

func (f *fakeRepo) GetOverrides(_ context.Context, _ uuid.UUID) (map[string]bool, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make(map[string]bool, len(f.overrides))
	for k, v := range f.overrides {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRepo) SetOverride(_ context.Context, _ uuid.UUID, feature string, enabled bool, _ *uuid.UUID) error {
	f.setCalls++
	if f.overrides == nil {
		f.overrides = map[string]bool{}
	}
	f.overrides[feature] = enabled
	return nil
}

func (f *fakeRepo) DeleteOverride(_ context.Context, _ uuid.UUID, feature string) error {
	f.delCalls++
	delete(f.overrides, feature)
	return nil
}

// fakeTier returns a fixed tier (or error).
type fakeTier struct {
	tier string
	err  error
}

func (f fakeTier) Tier(_ context.Context, _ uuid.UUID) (string, error) {
	return f.tier, f.err
}

func TestDefaultsForTier_ProgressiveDisclosure(t *testing.T) {
	cases := []struct {
		tier         string
		wantEnabled  []string
		wantDisabled []string
	}{
		{
			tier:         billing.TierFree,
			wantEnabled:  []string{FeatureFolders, FeatureFiles, FeatureShareLinks, FeatureBasicSearch},
			wantDisabled: []string{FeatureSSO, FeatureWebhooks, FeatureStrictZK, FeatureCMK, FeatureAISummaries, FeatureKChat},
		},
		{
			// Starter shares the Free baseline.
			tier:         billing.TierStarter,
			wantEnabled:  []string{FeatureFolders, FeatureBasicSearch},
			wantDisabled: []string{FeatureAuditLog, FeatureOnlyOffice, FeatureStrictZK},
		},
		{
			tier:         billing.TierBusiness,
			wantEnabled:  []string{FeatureSSO, FeatureAuditLog, FeatureRetentionPolicies, FeatureOnlyOffice, FeatureClientRooms, FeatureWebhooks, FeatureKChat, FeatureFiles},
			wantDisabled: []string{FeatureStrictZK, FeatureCMK, FeatureDataResidency, FeatureAISummaries},
		},
		{
			tier:        billing.TierSecureBusiness,
			wantEnabled: []string{FeatureStrictZK, FeatureCMK, FeatureDataResidency, FeatureAISummaries, FeatureSSO, FeatureFolders},
		},
		{
			// Unknown tier must fail-closed to the Free baseline.
			tier:         "enterprise-custom",
			wantEnabled:  []string{FeatureFolders, FeatureFiles},
			wantDisabled: []string{FeatureSSO, FeatureStrictZK},
		},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			got := DefaultsForTier(tc.tier)
			if len(got) != len(AllFeatures) {
				t.Fatalf("DefaultsForTier(%q) returned %d keys, want %d", tc.tier, len(got), len(AllFeatures))
			}
			for _, f := range tc.wantEnabled {
				if !got[f] {
					t.Errorf("tier %q: feature %q should be enabled by default", tc.tier, f)
				}
			}
			for _, f := range tc.wantDisabled {
				if got[f] {
					t.Errorf("tier %q: feature %q should be disabled by default", tc.tier, f)
				}
			}
		})
	}
}

func TestActiveFeatures_OverridesLayerOnTierDefaults(t *testing.T) {
	ws := uuid.New()
	repo := &fakeRepo{overrides: map[string]bool{
		// Beta-access: enable a Secure-Business feature on a Business ws.
		FeatureAISummaries: true,
		// Kill-switch: disable a feature that's on by default for the tier.
		FeatureWebhooks: false,
	}}
	svc := NewService(repo, fakeTier{tier: billing.TierBusiness})

	features, tier, err := svc.ActiveFeatures(context.Background(), ws)
	if err != nil {
		t.Fatalf("ActiveFeatures: %v", err)
	}
	if tier != billing.TierBusiness {
		t.Fatalf("tier = %q, want business", tier)
	}
	if !features[FeatureAISummaries] {
		t.Errorf("override should enable ai_summaries on a business workspace")
	}
	if features[FeatureWebhooks] {
		t.Errorf("override should disable webhooks despite business default")
	}
	// Untouched defaults remain.
	if !features[FeatureSSO] {
		t.Errorf("sso should remain enabled (business default, no override)")
	}
	if features[FeatureCMK] {
		t.Errorf("cmk should remain disabled (no override, not a business default)")
	}
}

func TestActiveFeatures_UnknownOverrideKeyIgnored(t *testing.T) {
	repo := &fakeRepo{overrides: map[string]bool{
		"some_removed_feature": true,
	}}
	svc := NewService(repo, fakeTier{tier: billing.TierFree})
	features, _, err := svc.ActiveFeatures(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ActiveFeatures: %v", err)
	}
	if _, ok := features["some_removed_feature"]; ok {
		t.Errorf("unknown override key leaked into the feature map")
	}
}

func TestActiveFeatures_RepoErrorPropagates(t *testing.T) {
	repo := &fakeRepo{getErr: errors.New("db down")}
	svc := NewService(repo, fakeTier{tier: billing.TierBusiness})
	if _, _, err := svc.ActiveFeatures(context.Background(), uuid.New()); err == nil {
		t.Fatalf("expected error when repo fails")
	}
}

func TestActiveFeatures_TierResolveErrorFailsClosed(t *testing.T) {
	// A billing lookup failure must degrade to the Free baseline, not error
	// (feature gating must never hard-fail a request) and not unlock paid UI.
	repo := &fakeRepo{}
	svc := NewService(repo, fakeTier{err: errors.New("billing unavailable")})
	features, tier, err := svc.ActiveFeatures(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ActiveFeatures should not error on tier-resolve failure: %v", err)
	}
	if tier != billing.TierFree {
		t.Fatalf("tier should fall back to free, got %q", tier)
	}
	if features[FeatureSSO] || features[FeatureStrictZK] {
		t.Errorf("paid features must stay disabled when tier resolution fails")
	}
	if !features[FeatureFolders] {
		t.Errorf("baseline features must stay enabled")
	}
}

func TestIsEnabled(t *testing.T) {
	ws := uuid.New()
	repo := &fakeRepo{overrides: map[string]bool{FeatureWebhooks: false}}
	svc := NewService(repo, fakeTier{tier: billing.TierBusiness})

	if !svc.IsEnabled(context.Background(), ws, FeatureSSO) {
		t.Errorf("sso should be enabled for business")
	}
	if svc.IsEnabled(context.Background(), ws, FeatureWebhooks) {
		t.Errorf("webhooks override should disable it")
	}
	if svc.IsEnabled(context.Background(), ws, FeatureStrictZK) {
		t.Errorf("strict_zk should be disabled for business")
	}
	if svc.IsEnabled(context.Background(), ws, "not_a_feature") {
		t.Errorf("unknown feature must be disabled (fail-closed)")
	}
}

func TestIsEnabled_RepoErrorFallsBackToTierDefault(t *testing.T) {
	repo := &fakeRepo{getErr: errors.New("db down")}
	svc := NewService(repo, fakeTier{tier: billing.TierBusiness})

	// A feature enabled by the tier default must stay on when the override
	// lookup fails — a transient workspace_features outage must not disable
	// baseline/entitled features.
	if !svc.IsEnabled(context.Background(), uuid.New(), FeatureSSO) {
		t.Errorf("override-read error must fall back to the tier default (sso on for business)")
	}
	if !svc.IsEnabled(context.Background(), uuid.New(), FeatureFolders) {
		t.Errorf("override-read error must keep baseline features (folders) enabled")
	}
	// A feature the tier does not grant stays off: the fallback can never
	// escalate above the tier ceiling.
	if svc.IsEnabled(context.Background(), uuid.New(), FeatureStrictZK) {
		t.Errorf("override-read error must not enable a feature above the tier (strict_zk)")
	}
}

func TestNilService_FreeDefaults(t *testing.T) {
	var svc *Service
	features, tier, err := svc.ActiveFeatures(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("nil service ActiveFeatures: %v", err)
	}
	if tier != billing.TierFree {
		t.Fatalf("nil service should resolve to free, got %q", tier)
	}
	if !features[FeatureFolders] || features[FeatureSSO] {
		t.Errorf("nil service should serve free defaults")
	}
	if svc.IsEnabled(context.Background(), uuid.New(), FeatureSSO) {
		t.Errorf("nil service IsEnabled should be false for paid features")
	}
}

func TestSetAndClearOverride_ValidatesKey(t *testing.T) {
	repo := &fakeRepo{}
	svc := NewService(repo, fakeTier{tier: billing.TierFree})
	ctx := context.Background()

	if err := svc.SetOverride(ctx, uuid.New(), "bogus", true, nil); err == nil {
		t.Errorf("SetOverride should reject unknown feature keys")
	}
	if err := svc.SetOverride(ctx, uuid.New(), FeatureSSO, true, nil); err != nil {
		t.Fatalf("SetOverride valid key: %v", err)
	}
	if repo.setCalls != 1 {
		t.Errorf("expected one repo SetOverride call, got %d", repo.setCalls)
	}
	if err := svc.ClearOverride(ctx, uuid.New(), FeatureSSO); err != nil {
		t.Fatalf("ClearOverride: %v", err)
	}
	if repo.delCalls != 1 {
		t.Errorf("expected one repo DeleteOverride call, got %d", repo.delCalls)
	}
}

func TestNewBillingTierResolver_NilBillingResolvesFree(t *testing.T) {
	r := NewBillingTierResolver(nil)
	tier, err := r.Tier(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("nil billing resolver: %v", err)
	}
	if tier != billing.TierFree {
		t.Fatalf("nil billing should resolve free, got %q", tier)
	}
}

func TestAllFeaturesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range AllFeatures {
		if seen[f] {
			t.Errorf("duplicate feature key %q in AllFeatures", f)
		}
		seen[f] = true
		if !IsKnownFeature(f) {
			t.Errorf("IsKnownFeature(%q) = false for a key in AllFeatures", f)
		}
	}
}
