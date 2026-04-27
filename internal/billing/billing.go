// Package billing tracks workspace plans and usage. Plans are stored
// in workspace_plans (one row per workspace, tier + optional override
// limits) and usage is appended to usage_events as a lightweight
// ledger. The service layer enforces quotas before storage / user /
// bandwidth-consuming operations.
//
// Stripe / external billing is intentionally out of scope here: the
// package exposes a stable Service API so a future Stripe webhook
// can call UpsertPlan with a tier change and the rest of the system
// transparently picks up the new limits.
package billing

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Tier names are stored as TEXT in workspace_plans so adding a new
// tier doesn't require a migration. The string values are the
// canonical identifiers used by both the API and the UI.
const (
	TierFree           = "free"
	TierStarter        = "starter"
	TierBusiness       = "business"
	TierSecureBusiness = "secure_business"
)

// Event types recorded in usage_events. Kept stable and short — the
// HTTP handlers compare them as exact strings.
const (
	EventStorage   = "storage"
	EventBandwidth = "bandwidth"
	EventUserAdded = "user_added"
)

// ErrPlanNotFound is returned by Repository.GetPlan when no row
// exists for a workspace. The service layer treats this as "fall
// back to the free-tier defaults" rather than as a hard error so
// new workspaces start with sensible limits.
var ErrPlanNotFound = errors.New("billing: plan not found")

// ErrQuotaExceeded is returned by the quota-check methods when the
// caller would push the workspace past its limit. Handlers map this
// to 402 Payment Required so the frontend can prompt the user to
// upgrade.
var ErrQuotaExceeded = errors.New("billing: plan quota exceeded")

// Plan describes a workspace's billing tier and effective limits.
// Limits stored in the workspace_plans row override the per-tier
// defaults when non-nil; nil values fall back to TierDefaults below.
type Plan struct {
	ID                       uuid.UUID `json:"id"`
	WorkspaceID              uuid.UUID `json:"workspace_id"`
	Tier                     string    `json:"tier"`
	MaxStorageBytes          *int64    `json:"max_storage_bytes,omitempty"`
	MaxUsers                 *int      `json:"max_users,omitempty"`
	MaxBandwidthBytesMonthly *int64    `json:"max_bandwidth_bytes_monthly,omitempty"`
	// StripeCustomerID is set after the workspace's first successful
	// Stripe Checkout completion. UpsertPlan preserves the existing
	// value when this field is nil so a webhook-driven plan change
	// doesn't accidentally clear the linkage.
	StripeCustomerID *string   `json:"stripe_customer_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// UsageEvent is one row of the usage_events ledger.
type UsageEvent struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	EventType   string    `json:"event_type"`
	Bytes       int64     `json:"bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

// Limits is the resolved view of a plan after applying defaults. All
// values are non-nil — callers don't need to chase pointers.
type Limits struct {
	Tier                     string
	MaxStorageBytes          int64
	MaxUsers                 int
	MaxBandwidthBytesMonthly int64
}

// TierDefaults maps a tier name to its baseline limits. Values come
// from PROPOSAL.md §2.3:
//   - Free: 5 GB total, 5 users, 10 GB/month bandwidth.
//   - Starter: 10 GB / user pooled, 25 users, 100 GB/month bandwidth.
//   - Business: 50 GB / user pooled, 250 users, 1 TB/month bandwidth.
//   - Secure Business: configurable; we set generous defaults here so
//     an unconfigured secure_business workspace still functions while
//     ops sets bespoke values via UpsertPlan.
var TierDefaults = map[string]Limits{
	TierFree: {
		Tier:                     TierFree,
		MaxStorageBytes:          5 * gigabyte,
		MaxUsers:                 5,
		MaxBandwidthBytesMonthly: 10 * gigabyte,
	},
	TierStarter: {
		Tier:                     TierStarter,
		MaxStorageBytes:          250 * gigabyte,
		MaxUsers:                 25,
		MaxBandwidthBytesMonthly: 100 * gigabyte,
	},
	TierBusiness: {
		Tier:                     TierBusiness,
		MaxStorageBytes:          1000 * gigabyte,
		MaxUsers:                 250,
		MaxBandwidthBytesMonthly: 1000 * gigabyte,
	},
	TierSecureBusiness: {
		Tier:                     TierSecureBusiness,
		MaxStorageBytes:          5000 * gigabyte,
		MaxUsers:                 1000,
		MaxBandwidthBytesMonthly: 5000 * gigabyte,
	},
}

const gigabyte int64 = 1024 * 1024 * 1024

// IsValidTier returns true when t is one of the canonical tier names.
func IsValidTier(t string) bool {
	switch t {
	case TierFree, TierStarter, TierBusiness, TierSecureBusiness:
		return true
	}
	return false
}

// EffectiveLimits resolves the effective limit set for a plan,
// substituting per-tier defaults for any nil column.
func (p *Plan) EffectiveLimits() Limits {
	d, ok := TierDefaults[p.Tier]
	if !ok {
		d = TierDefaults[TierFree]
	}
	out := d
	out.Tier = p.Tier
	if p.MaxStorageBytes != nil {
		out.MaxStorageBytes = *p.MaxStorageBytes
	}
	if p.MaxUsers != nil {
		out.MaxUsers = *p.MaxUsers
	}
	if p.MaxBandwidthBytesMonthly != nil {
		out.MaxBandwidthBytesMonthly = *p.MaxBandwidthBytesMonthly
	}
	return out
}

// DefaultLimitsFor returns the baseline limits for tier t, or the
// free-tier defaults when t is unknown. Used by the service when
// no plan row exists yet.
func DefaultLimitsFor(t string) Limits {
	d, ok := TierDefaults[t]
	if !ok {
		return TierDefaults[TierFree]
	}
	return d
}
