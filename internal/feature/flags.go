// Package feature implements progressive feature disclosure for ZK Drive.
//
// A workspace's available feature set is derived from two layers:
//
//  1. Tier defaults — every billing tier (free/starter, business,
//     secure_business) ships with a baseline set of features turned on.
//     This is the "progressive disclosure" backbone: a Free/Starter
//     workspace only sees folders, files, share links and basic search,
//     so the UI for SSO, audit logs, retention policies, ONLYOFFICE,
//     client rooms, webhooks, strict-ZK, CMK, data residency and AI
//     summaries stays hidden until the workspace upgrades.
//
//  2. Per-workspace overrides — rows in the workspace_features table let
//     an operator (or a future admin UI) explicitly flip a single feature
//     on or off for one workspace, regardless of its tier default. This
//     supports beta access, contractual carve-outs, and incident
//     kill-switches without a tier change or a deploy.
//
// The effective state of a feature is: override if one exists, else the
// tier default, else false (unknown features are disabled — fail-closed).
//
// Feature keys are stable TEXT identifiers shared verbatim with the
// frontend (frontend/src/hooks/useFeatures.ts). They are intentionally
// NOT an enum so adding a feature is a code-only change in this file plus
// the UI, with no migration.
package feature

import "github.com/kennguy3n/zk-drive/internal/billing"

// Feature keys. These strings are the contract between the backend
// feature service and the frontend useFeatures() hook — keep them in
// sync with frontend/src/hooks/useFeatures.ts.
const (
	// Baseline features available on every tier, including Free/Starter.
	FeatureFolders     = "folders"
	FeatureFiles       = "files"
	FeatureShareLinks  = "share_links"
	FeatureBasicSearch = "basic_search"

	// Business-tier features.
	FeatureSSO               = "sso"
	FeatureAuditLog          = "audit_log"
	FeatureRetentionPolicies = "retention_policies"
	FeatureOnlyOffice        = "onlyoffice"
	FeatureClientRooms       = "client_rooms"
	FeatureWebhooks          = "webhooks"
	FeatureKChat             = "kchat"

	// Secure-Business-tier features.
	FeatureStrictZK      = "strict_zk"
	FeatureCMK           = "cmk"
	FeatureDataResidency = "data_residency"
	FeatureAISummaries   = "ai_summaries"
)

// AllFeatures is the canonical list of every feature key the system knows
// about. ActiveFeatures iterates this so the GET /api/features response
// always reports the full set (each as enabled/disabled) rather than only
// the enabled ones — the frontend can then reason about a fixed schema
// instead of treating an absent key as ambiguous.
//
// The slice is grouped baseline → business → secure for readability. (The
// JSON payload key order is not governed by this slice: encoding/json
// always marshals map keys sorted alphabetically, which is deterministic
// regardless.)
var AllFeatures = []string{
	FeatureFolders,
	FeatureFiles,
	FeatureShareLinks,
	FeatureBasicSearch,
	FeatureSSO,
	FeatureAuditLog,
	FeatureRetentionPolicies,
	FeatureOnlyOffice,
	FeatureClientRooms,
	FeatureWebhooks,
	FeatureKChat,
	FeatureStrictZK,
	FeatureDataResidency,
	FeatureCMK,
	FeatureAISummaries,
}

// featureSet is a set of feature keys, used to build the cumulative tier
// defaults below.
type featureSet map[string]struct{}

func setOf(keys ...string) featureSet {
	s := make(featureSet, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

func (s featureSet) with(keys ...string) featureSet {
	out := make(featureSet, len(s)+len(keys))
	for k := range s {
		out[k] = struct{}{}
	}
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out
}

// Baseline features for Free/Starter: folders, files, share links and
// basic search only — everything else is hidden until the workspace
// upgrades. (Workstream 4.3.)
var baselineFeatures = setOf(
	FeatureFolders,
	FeatureFiles,
	FeatureShareLinks,
	FeatureBasicSearch,
)

// Business adds collaboration + governance features on top of the
// baseline: SSO, audit log, retention policies, ONLYOFFICE, client
// rooms, webhooks, and KChat team chat.
var businessFeatures = baselineFeatures.with(
	FeatureSSO,
	FeatureAuditLog,
	FeatureRetentionPolicies,
	FeatureOnlyOffice,
	FeatureClientRooms,
	FeatureWebhooks,
	FeatureKChat,
)

// Secure Business adds the privacy/compliance controls on top of
// Business: strict-ZK folders, customer-managed keys, data-residency
// controls, and AI summaries.
var secureBusinessFeatures = businessFeatures.with(
	FeatureStrictZK,
	FeatureCMK,
	FeatureDataResidency,
	FeatureAISummaries,
)

// tierDefaults maps a billing tier to the feature set it enables by
// default. Free and Starter share the same baseline (the workstream
// groups them together as "Free/Starter").
var tierDefaults = map[string]featureSet{
	billing.TierFree:           baselineFeatures,
	billing.TierStarter:        baselineFeatures,
	billing.TierBusiness:       businessFeatures,
	billing.TierSecureBusiness: secureBusinessFeatures,
}

// normalizeTier maps an arbitrary tier string to a tier we have defaults
// for. Unknown / empty tiers fall back to Free — the most restrictive
// set — so a misconfigured or future tier never accidentally unlocks
// paid features (fail-closed).
func normalizeTier(tier string) string {
	if _, ok := tierDefaults[tier]; ok {
		return tier
	}
	return billing.TierFree
}

// DefaultEnabled reports whether feature is on by default for the given
// billing tier, before any per-workspace override is applied. Unknown
// features are disabled.
func DefaultEnabled(tier, feature string) bool {
	set := tierDefaults[normalizeTier(tier)]
	_, ok := set[feature]
	return ok
}

// DefaultsForTier returns the full enabled/disabled map for a tier across
// every known feature. Used to compute the response when no per-workspace
// overrides exist, and as the base that overrides are layered onto.
func DefaultsForTier(tier string) map[string]bool {
	out := make(map[string]bool, len(AllFeatures))
	for _, f := range AllFeatures {
		out[f] = DefaultEnabled(tier, f)
	}
	return out
}

// IsKnownFeature reports whether key is a feature the system recognises.
// Used to reject writes of arbitrary override keys at the service layer.
func IsKnownFeature(key string) bool {
	for _, f := range AllFeatures {
		if f == key {
			return true
		}
	}
	return false
}
