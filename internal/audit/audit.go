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
	ActionPermissionGrant    = "permission.grant"
	ActionPermissionRevoke   = "permission.revoke"
	ActionAdminUserInvite    = "admin.user_invite"
	ActionAdminUserDeactivate = "admin.user_deactivate"
	ActionAdminUserRoleChange = "admin.user_role_change"
	ActionWorkspaceUpdate    = "workspace.update"
	ActionRetentionPolicyUpsert = "retention.policy_upsert"
	ActionRetentionPolicyDelete = "retention.policy_delete"
	ActionAdminBillingUpdate    = "admin.billing_update"
	ActionAdminBillingCheckout  = "admin.billing_checkout_session"
	ActionAdminBillingPortal    = "admin.billing_portal_session"
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
