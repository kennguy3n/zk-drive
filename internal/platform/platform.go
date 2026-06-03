// Package platform implements the control-plane "super-API" that
// operations uses to manage the whole fleet of workspaces (tenants):
// automated provisioning, suspension / resumption, fleet-wide usage
// reporting, billing reconciliation, and usage alerting.
//
// It deliberately sits OUTSIDE the per-workspace request path: callers
// authenticate with a platform API key (see apikey.go) rather than a
// workspace JWT, and the service queries across every tenant without
// binding the app.workspace_id GUC, so the migration-024 row-level
// security policies fall through to their "no restriction" bypass
// branch. The package therefore must never be reachable from the
// tenant-scoped middleware chain.
package platform

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a workspace, alert rule, or API key
// referenced by id does not exist. Handlers map it to 404.
var ErrNotFound = errors.New("platform: not found")

// ErrInvalidArgument is returned for caller-supplied input that fails
// validation (empty name, unknown tier, bad metric / operator).
// Handlers map it to 400.
var ErrInvalidArgument = errors.New("platform: invalid argument")

// Alert metric identifiers recognised by EvaluateUsageAlerts. Stored
// as free-form TEXT in usage_alert_rules.metric so adding one is a
// code-only change.
const (
	MetricStoragePercent     = "storage_percent"
	MetricUserCount          = "user_count"
	MetricBandwidthMonthlyGB = "bandwidth_monthly_gb"
)

// Alert comparison operators.
const (
	OperatorGTE = "gte"
	OperatorLTE = "lte"
)

// Provisioning sources recorded in workspaces.provisioned_by.
const (
	ProvisionedManual = "manual"
	ProvisionedAPI    = "api"
	ProvisionedStripe = "stripe"
)

// WorkspaceSummary is the list-view projection of a workspace returned
// by ListWorkspaces. It joins the workspace row with derived counts
// (users) and billing limits (storage quota) so the admin UI can
// render a fleet table without N follow-up requests.
type WorkspaceSummary struct {
	ID                uuid.UUID  `json:"id"`
	Name              string     `json:"name"`
	Tier              string     `json:"tier"`
	UserCount         int        `json:"user_count"`
	StorageUsedBytes  int64      `json:"storage_used_bytes"`
	StorageQuotaBytes int64      `json:"storage_quota_bytes"`
	StoragePercent    float64    `json:"storage_percent"`
	ProvisionedBy     string     `json:"provisioned_by"`
	Suspended         bool       `json:"suspended"`
	SuspendedAt       *time.Time `json:"suspended_at,omitempty"`
	SuspensionReason  string     `json:"suspension_reason,omitempty"`
	LastActiveAt      *time.Time `json:"last_active_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// ListFilters narrows and paginates ListWorkspaces. Zero values mean
// "no filter" except Limit, which falls back to DefaultListLimit when
// <= 0. All filters combine with AND.
type ListFilters struct {
	// Tier, when non-empty, restricts to workspaces whose billing
	// plan tier equals this value.
	Tier string
	// Suspended, when non-nil, restricts to suspended (true) or
	// active (false) workspaces.
	Suspended *bool
	// MinStoragePercent / MaxStoragePercent bound storage_used /
	// quota * 100. MaxStoragePercent <= 0 means "no upper bound".
	MinStoragePercent float64
	MaxStoragePercent float64
	// CreatedAfter / CreatedBefore bound workspaces.created_at.
	CreatedAfter  *time.Time
	CreatedBefore *time.Time

	Limit  int
	Offset int
}

// DefaultListLimit is the page size applied when ListFilters.Limit is
// not set. MaxListLimit caps an over-large caller request so a single
// page can't scan the whole fleet.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// UsageReport is the detailed per-workspace usage breakdown returned
// by GetWorkspaceUsage.
type UsageReport struct {
	WorkspaceID         uuid.UUID `json:"workspace_id"`
	Tier                string    `json:"tier"`
	StorageUsedBytes    int64     `json:"storage_used_bytes"`
	StorageQuotaBytes   int64     `json:"storage_quota_bytes"`
	StoragePercent      float64   `json:"storage_percent"`
	FileCount           int64     `json:"file_count"`
	PreviewCount        int64     `json:"preview_count"`
	BandwidthMTDBytes   int64     `json:"bandwidth_mtd_bytes"`
	BandwidthLimitBytes int64     `json:"bandwidth_limit_bytes"`
	UserCount           int       `json:"user_count"`
	GeneratedAt         time.Time `json:"generated_at"`
}

// ReconcileReport summarises a BulkReconcileBilling pass over the
// fleet. Mismatches lists every workspace whose local plan disagrees
// with the upstream (Stripe) subscription state.
type ReconcileReport struct {
	WorkspacesScanned int              `json:"workspaces_scanned"`
	Mismatches        []ReconcileEntry `json:"mismatches"`
	GeneratedAt       time.Time        `json:"generated_at"`
}

// ReconcileEntry is one workspace flagged by BulkReconcileBilling.
type ReconcileEntry struct {
	WorkspaceID      uuid.UUID `json:"workspace_id"`
	LocalTier        string    `json:"local_tier"`
	StripeCustomerID string    `json:"stripe_customer_id,omitempty"`
	StripeStatus     string    `json:"stripe_status,omitempty"`
	StripeTier       string    `json:"stripe_tier,omitempty"`
	Reason           string    `json:"reason"`
}

// AlertRule mirrors a usage_alert_rules row.
type AlertRule struct {
	ID              uuid.UUID  `json:"id"`
	WorkspaceID     *uuid.UUID `json:"workspace_id,omitempty"`
	Metric          string     `json:"metric"`
	Threshold       float64    `json:"threshold"`
	Operator        string     `json:"operator"`
	WebhookURL      string     `json:"webhook_url,omitempty"`
	Email           string     `json:"email,omitempty"`
	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// AlertFiring records a rule that crossed its threshold during an
// EvaluateUsageAlerts pass, along with the observed value and which
// notification channels were dispatched.
type AlertFiring struct {
	RuleID       uuid.UUID `json:"rule_id"`
	WorkspaceID  uuid.UUID `json:"workspace_id"`
	Metric       string    `json:"metric"`
	Operator     string    `json:"operator"`
	Threshold    float64   `json:"threshold"`
	Value        float64   `json:"value"`
	WebhookFired bool      `json:"webhook_fired"`
	EmailFired   bool      `json:"email_fired"`
	FiredAt      time.Time `json:"fired_at"`
}

// thresholdCrossed reports whether value satisfies the rule's
// operator/threshold comparison. Unknown operators never fire (fail
// safe — a malformed rule should not spam alerts).
func thresholdCrossed(operator string, value, threshold float64) bool {
	switch operator {
	case OperatorGTE:
		return value >= threshold
	case OperatorLTE:
		return value <= threshold
	default:
		return false
	}
}

// validMetric reports whether m is a metric EvaluateUsageAlerts knows
// how to compute.
func validMetric(m string) bool {
	switch m {
	case MetricStoragePercent, MetricUserCount, MetricBandwidthMonthlyGB:
		return true
	}
	return false
}

// validOperator reports whether o is a recognised comparison operator.
func validOperator(o string) bool {
	return o == OperatorGTE || o == OperatorLTE
}
