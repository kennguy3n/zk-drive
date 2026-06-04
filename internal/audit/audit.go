// Package audit implements a fire-and-forget security audit log for
// login / logout, SSO link, permission grant / revoke, admin user
// management and workspace settings changes. It is distinct from
// internal/activity: activity_log is user-facing (feeds the in-app
// activity stream), audit_log is admin-only and retained for
// compliance reporting.
package audit

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Action constants name the events recorded in audit_log. These
// strings are dotted so downstream tooling can group by prefix without
// parsing multiple columns.
const (
	ActionLogin              = "auth.login"
	ActionLogout             = "auth.logout"
	ActionPasswordChange     = "auth.password_change"
	ActionSSOLink            = "auth.sso_link"
	ActionSSOLogin           = "auth.sso_login"
	// MFA / TOTP lifecycle. All five are user-visible in
	// the workspace audit log and surface in operator monitoring
	// for unusual frequency (e.g. repeated disable/enroll churn,
	// recovery-code consumption spikes that hint at lost devices).
	ActionMFAEnroll        = "auth.mfa_enroll"
	ActionMFAVerify        = "auth.mfa_verify"
	ActionMFARecoveryUse   = "auth.mfa_recovery_use"
	ActionMFADisable       = "auth.mfa_disable"
	ActionMFAPolicyChange  = "auth.mfa_policy_change"
	ActionPermissionGrant    = "permission.grant"
	ActionPermissionRevoke   = "permission.revoke"
	ActionAdminUserInvite    = "admin.user_invite"
	ActionAdminUserDeactivate = "admin.user_deactivate"
	ActionAdminUserRoleChange = "admin.user_role_change"
	ActionWorkspaceCreate          = "workspace.create"
	ActionWorkspaceUpdate          = "workspace.update"
	ActionWorkspaceSearchLanguage  = "workspace.search_language_change"
	ActionRetentionPolicyUpsert = "retention.policy_upsert"
	ActionRetentionPolicyDelete = "retention.policy_delete"
	ActionAdminBillingUpdate    = "admin.billing_update"
	ActionAdminBillingCheckout  = "admin.billing_checkout_session"
	ActionAdminBillingPortal    = "admin.billing_portal_session"
	// JWT signing-key rotation. Recorded whenever an admin rotates
	// the platform's ES256 session-signing key (POST
	// /api/admin/jwt/rotate); the metadata blob carries the new
	// key id and algorithm so a rotation can be correlated with the
	// jwt_signing_keys table without exposing key material.
	ActionAdminJWTRotate = "admin.jwt_rotate"
	// Guest-invite delivery. Recorded on the same audit_log
	// stream as the auth + permission events so operators can join
	// "invite created → email delivered" on resource_id and surface
	// undelivered invites in compliance reports. The metadata blob
	// carries the SendOutcome (`ok` / `smtp_error` /
	// `template_error` / `address_invalid` / `disabled`) so a
	// failed delivery is still visible without dropping the row.
	ActionGuestInviteEmailed = "sharing.guest_invite_emailed"
	// Outbound webhook subscription lifecycle. Workspace
	// admins create / pause / resume / delete subscriptions; each
	// transition is audited so compliance reviews can attribute
	// fan-out changes to a specific operator.
	ActionWebhookSubscriptionCreate = "webhooks.subscription_create"
	ActionWebhookSubscriptionDelete = "webhooks.subscription_delete"
	ActionWebhookSubscriptionResume = "webhooks.subscription_resume"
	ActionWebhookSubscriptionTest   = "webhooks.subscription_test"
	// Per-workspace IP allowlisting (conditional access). Admins
	// add / remove CIDR rules and flip the master switch; each
	// change is audited so a compliance reviewer can attribute a
	// network-access policy change to a specific operator and time.
	ActionIPAllowRuleAdd      = "workspace.ip_allowlist_rule_add"
	ActionIPAllowRuleRemove   = "workspace.ip_allowlist_rule_remove"
	ActionIPAllowPolicyChange = "workspace.ip_allowlist_policy_change"
)

// Entry mirrors a single audit_log row. ActorID is nullable because
// some events (e.g. failed login with unknown email) do not have a
// resolved actor.
type Entry struct {
	ID           uuid.UUID       `json:"id"`
	WorkspaceID  uuid.UUID       `json:"workspace_id"`
	ActorID      *uuid.UUID      `json:"actor_id,omitempty"`
	Action       string          `json:"action"`
	ResourceType *string         `json:"resource_type,omitempty"`
	ResourceID   *uuid.UUID      `json:"resource_id,omitempty"`
	IPAddress    *string         `json:"ip_address,omitempty"`
	UserAgent    *string         `json:"user_agent,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}
