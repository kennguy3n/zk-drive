-- Usage alerting rules for the platform control plane.
--
-- Operators (and, in a future self-serve UI, workspace admins) register rules
-- that fire a webhook / email when a workspace metric crosses a threshold.
-- EvaluateUsageAlerts (internal/platform) walks every rule, computes the
-- current value of the named metric for the rule's workspace, and fires the
-- configured channel when the comparison holds.
--
-- metric is one of:
--   'storage_percent'      — storage_used_bytes / plan storage limit * 100
--   'user_count'           — active (non-deactivated) users in the workspace
--   'bandwidth_monthly_gb' — month-to-date download bandwidth, in GiB
-- Stored as TEXT (no CHECK) so adding a metric is a code-only change.
--
-- operator is 'gte' (fire when value >= threshold, the default) or 'lte'.
-- threshold is NUMERIC so percentage / fractional thresholds round-trip
-- exactly. last_triggered_at records the most recent firing so the evaluator
-- (and the UI) can avoid alert storms; it is informational here and the
-- de-dup window is enforced in application code.
--
-- workspace_id ON DELETE CASCADE drops a workspace's rules with it. The table
-- is intentionally outside the tenant_isolation RLS set (migration 024): the
-- platform evaluator runs without an app.workspace_id GUC and must see rules
-- across every tenant.
CREATE TABLE usage_alert_rules (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID REFERENCES workspaces(id) ON DELETE CASCADE,
    metric            TEXT NOT NULL,
    threshold         NUMERIC NOT NULL,
    operator          TEXT NOT NULL DEFAULT 'gte',
    webhook_url       TEXT,
    email             TEXT,
    last_triggered_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_usage_alert_rules_workspace ON usage_alert_rules(workspace_id);
