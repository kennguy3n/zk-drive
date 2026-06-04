package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/zk-drive/internal/billing"
)

const gibibyte = 1024 * 1024 * 1024

// defaultAlertCooldown is the minimum interval between successive
// firings of the same usage-alert rule. It bounds alert storms when
// EvaluateUsageAlerts is driven on a schedule while a threshold stays
// crossed. Override per-service with WithAlertCooldown (0 disables).
const defaultAlertCooldown = time.Hour

// ListAlertRules returns every usage-alert rule, newest first.
func (s *PlatformService) ListAlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, metric, threshold, operator, webhook_url, email, last_triggered_at, created_at
         FROM usage_alert_rules ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("platform: list alert rules: %w", err)
	}
	defer rows.Close()
	out := make([]AlertRule, 0)
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("platform: iterate alert rules: %w", err)
	}
	return out, nil
}

// CreateAlertRule validates and inserts a usage-alert rule.
// workspace_id is optional at the schema level, but the evaluator only
// fires rules with a target workspace, so creation requires one.
func (s *PlatformService) CreateAlertRule(ctx context.Context, rule AlertRule) (*AlertRule, error) {
	rule.Metric = strings.TrimSpace(rule.Metric)
	rule.Operator = strings.TrimSpace(rule.Operator)
	if rule.Operator == "" {
		rule.Operator = OperatorGTE
	}
	if rule.WorkspaceID == nil {
		return nil, fmt.Errorf("%w: workspace_id is required", ErrInvalidArgument)
	}
	if !validMetric(rule.Metric) {
		return nil, fmt.Errorf("%w: unknown metric %q", ErrInvalidArgument, rule.Metric)
	}
	if !validOperator(rule.Operator) {
		return nil, fmt.Errorf("%w: unknown operator %q", ErrInvalidArgument, rule.Operator)
	}
	if strings.TrimSpace(rule.WebhookURL) == "" && strings.TrimSpace(rule.Email) == "" {
		return nil, fmt.Errorf("%w: at least one of webhook_url or email is required", ErrInvalidArgument)
	}

	out, err := scanAlertRule(s.pool.QueryRow(ctx,
		`INSERT INTO usage_alert_rules (workspace_id, metric, threshold, operator, webhook_url, email)
         VALUES ($1, $2, $3, $4, $5, $6)
         RETURNING id, workspace_id, metric, threshold, operator, webhook_url, email, last_triggered_at, created_at`,
		rule.WorkspaceID, rule.Metric, rule.Threshold, rule.Operator,
		nullIfEmpty(strings.TrimSpace(rule.WebhookURL)), nullIfEmpty(strings.TrimSpace(rule.Email)),
	))
	if err != nil {
		return nil, fmt.Errorf("platform: insert alert rule: %w", err)
	}
	return &out, nil
}

// DeleteAlertRule removes a rule by id, returning ErrNotFound when no
// such rule exists.
func (s *PlatformService) DeleteAlertRule(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM usage_alert_rules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("platform: delete alert rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// EvaluateUsageAlerts walks every workspace-scoped rule, computes the
// current value of its metric, and fires the configured channels when
// the threshold is crossed. Rules without a target workspace are
// skipped (they cannot be evaluated against a concrete tenant). The
// returned slice records every firing, including which channels were
// dispatched.
func (s *PlatformService) EvaluateUsageAlerts(ctx context.Context) ([]AlertFiring, error) {
	rules, err := s.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	firings := make([]AlertFiring, 0)
	for _, rule := range rules {
		if rule.WorkspaceID == nil {
			continue
		}
		value, err := s.metricValue(ctx, *rule.WorkspaceID, rule.Metric)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Workspace vanished between listing and evaluation;
				// skip rather than abort the whole pass.
				continue
			}
			return nil, err
		}
		if !thresholdCrossed(rule.Operator, value, rule.Threshold) {
			continue
		}
		// De-dup: suppress re-firing a rule whose previous firing is
		// still within the cooldown window. This enforces the contract
		// documented on usage_alert_rules.last_triggered_at and stops a
		// cron-driven evaluation loop from storming the configured
		// channels on every tick while a threshold stays crossed.
		now := s.now()
		if rule.LastTriggeredAt != nil && s.alertCooldown > 0 &&
			now.Sub(*rule.LastTriggeredAt) < s.alertCooldown {
			continue
		}
		firing := AlertFiring{
			RuleID:      rule.ID,
			WorkspaceID: *rule.WorkspaceID,
			Metric:      rule.Metric,
			Operator:    rule.Operator,
			Threshold:   rule.Threshold,
			Value:       value,
			FiredAt:     now,
		}
		s.dispatchFiring(ctx, rule, &firing)
		// Stamp last_triggered_at with the service clock (not the DB
		// clock) so the cooldown above is evaluated on a single,
		// test-controllable timeline.
		if _, uerr := s.pool.Exec(ctx,
			`UPDATE usage_alert_rules SET last_triggered_at = $2 WHERE id = $1`, rule.ID, now,
		); uerr != nil {
			s.log().Warn("platform: update last_triggered_at failed", "rule_id", rule.ID, "err", uerr)
		}
		firings = append(firings, firing)
	}
	return firings, nil
}

// dispatchFiring sends the rule's configured notification channels via
// the dispatcher (when wired) and records which fired on the firing.
func (s *PlatformService) dispatchFiring(ctx context.Context, rule AlertRule, firing *AlertFiring) {
	if s.dispatcher == nil {
		return
	}
	// Hand every channel an identical, ordering-independent snapshot of
	// the alert: a copy with the cross-channel "fired" meta-flags zeroed.
	// Otherwise the email payload would observe WebhookFired=true (set by
	// the webhook branch above it) while the webhook payload never can,
	// so a channel's report of the *other* channel's status would depend
	// on dispatch order. The real per-channel results are recorded on
	// *firing for the returned AlertFiring.
	payload := *firing
	payload.WebhookFired = false
	payload.EmailFired = false
	if url := strings.TrimSpace(rule.WebhookURL); url != "" {
		if err := s.dispatcher.DispatchWebhook(ctx, url, payload); err != nil {
			s.log().Warn("platform: alert webhook dispatch failed", "rule_id", rule.ID, "err", err)
		} else {
			firing.WebhookFired = true
		}
	}
	if email := strings.TrimSpace(rule.Email); email != "" {
		if err := s.dispatcher.DispatchEmail(ctx, email, payload); err != nil {
			s.log().Warn("platform: alert email dispatch failed", "rule_id", rule.ID, "err", err)
		} else {
			firing.EmailFired = true
		}
	}
}

// metricValue computes the current value of metric for a workspace.
func (s *PlatformService) metricValue(ctx context.Context, workspaceID uuid.UUID, metric string) (float64, error) {
	switch metric {
	case MetricStoragePercent:
		var used, quota int64
		err := s.pool.QueryRow(ctx,
			`SELECT storage_used_bytes, storage_quota_bytes FROM workspaces WHERE id = $1`,
			workspaceID,
		).Scan(&used, &quota)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, ErrNotFound
			}
			return 0, fmt.Errorf("platform: load storage metric: %w", err)
		}
		return storagePercent(used, quota), nil
	case MetricUserCount:
		var n int64
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM users WHERE workspace_id = $1 AND deactivated_at IS NULL`,
			workspaceID,
		).Scan(&n); err != nil {
			return 0, fmt.Errorf("platform: load user-count metric: %w", err)
		}
		return float64(n), nil
	case MetricBandwidthMonthlyGB:
		var bytes int64
		if err := s.pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(bytes), 0) FROM usage_events
             WHERE workspace_id = $1 AND event_type = $2 AND created_at >= date_trunc('month', now())`,
			workspaceID, billing.EventBandwidth,
		).Scan(&bytes); err != nil {
			return 0, fmt.Errorf("platform: load bandwidth metric: %w", err)
		}
		return float64(bytes) / float64(gibibyte), nil
	default:
		return 0, fmt.Errorf("%w: unknown metric %q", ErrInvalidArgument, metric)
	}
}

// BulkReconcileBilling scans every workspace's plan and flags drift
// between the locally-stored tier and the upstream Stripe subscription.
// When a SubscriptionInspector is wired it compares live subscription
// state; otherwise it performs structural checks (a paid local tier
// without a linked Stripe customer, or a linked customer it cannot
// verify).
func (s *PlatformService) BulkReconcileBilling(ctx context.Context) (*ReconcileReport, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT w.id, COALESCE(p.tier, w.tier), p.stripe_customer_id
         FROM workspaces w
         LEFT JOIN workspace_plans p ON p.workspace_id = w.id`)
	if err != nil {
		return nil, fmt.Errorf("platform: scan workspaces for reconcile: %w", err)
	}
	defer rows.Close()

	report := &ReconcileReport{Mismatches: make([]ReconcileEntry, 0), GeneratedAt: s.now()}
	type wsPlan struct {
		id         uuid.UUID
		tier       string
		customerID *string
	}
	var plans []wsPlan
	for rows.Next() {
		var p wsPlan
		if err := rows.Scan(&p.id, &p.tier, &p.customerID); err != nil {
			return nil, fmt.Errorf("platform: scan plan row: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("platform: iterate plan rows: %w", err)
	}

	for _, p := range plans {
		report.WorkspacesScanned++
		entry, mismatch := s.reconcileOne(ctx, p.id, p.tier, p.customerID)
		if mismatch {
			report.Mismatches = append(report.Mismatches, entry)
		}
	}
	return report, nil
}

// reconcileOne evaluates a single workspace plan against Stripe and
// returns the mismatch entry plus whether a mismatch was found.
func (s *PlatformService) reconcileOne(ctx context.Context, workspaceID uuid.UUID, localTier string, customerID *string) (ReconcileEntry, bool) {
	entry := ReconcileEntry{WorkspaceID: workspaceID, LocalTier: localTier}
	hasCustomer := customerID != nil && strings.TrimSpace(*customerID) != ""
	if hasCustomer {
		entry.StripeCustomerID = strings.TrimSpace(*customerID)
	}

	if s.subscriptions == nil {
		// No live inspector: fall back to structural checks.
		if localTier != billing.TierFree && !hasCustomer {
			entry.Reason = "paid tier without linked stripe customer"
			return entry, true
		}
		if hasCustomer {
			entry.Reason = "stripe customer linked but subscription state unverified (inspector not configured)"
			return entry, true
		}
		return entry, false
	}

	if !hasCustomer {
		if localTier != billing.TierFree {
			entry.Reason = "paid tier without linked stripe customer"
			return entry, true
		}
		return entry, false
	}

	status, stripeTier, err := s.subscriptions.SubscriptionStatus(ctx, entry.StripeCustomerID)
	if err != nil {
		entry.Reason = fmt.Sprintf("stripe lookup failed: %v", err)
		return entry, true
	}
	entry.StripeStatus = status
	entry.StripeTier = stripeTier
	if status == "" {
		entry.Reason = "no stripe subscription found for linked customer"
		return entry, true
	}
	if stripeTier != "" && stripeTier != localTier {
		entry.Reason = "tier drift: local plan does not match stripe subscription"
		return entry, true
	}
	return entry, false
}

func scanAlertRule(row rowScanner) (AlertRule, error) {
	var (
		r          AlertRule
		webhookURL *string
		email      *string
	)
	if err := row.Scan(
		&r.ID, &r.WorkspaceID, &r.Metric, &r.Threshold, &r.Operator,
		&webhookURL, &email, &r.LastTriggeredAt, &r.CreatedAt,
	); err != nil {
		return AlertRule{}, err
	}
	if webhookURL != nil {
		r.WebhookURL = *webhookURL
	}
	if email != nil {
		r.Email = *email
	}
	return r, nil
}
