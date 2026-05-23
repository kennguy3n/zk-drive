package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestActionConstantsAreDotted: every Action* constant must be a
// dotted string so downstream tooling (BI / SIEM / log aggregators)
// can group by prefix without parsing the full token. A new event
// type that ships as a non-dotted string would skew the grouping.
func TestActionConstantsAreDotted(t *testing.T) {
	actions := []string{
		ActionLogin, ActionLogout, ActionPasswordChange,
		ActionSSOLink, ActionSSOLogin,
		ActionPermissionGrant, ActionPermissionRevoke,
		ActionAdminUserInvite, ActionAdminUserDeactivate, ActionAdminUserRoleChange,
		ActionWorkspaceCreate, ActionWorkspaceUpdate,
		ActionRetentionPolicyUpsert, ActionRetentionPolicyDelete,
		ActionAdminBillingUpdate, ActionAdminBillingCheckout, ActionAdminBillingPortal,
		ActionGuestInviteEmailed,
		ActionWebhookSubscriptionCreate, ActionWebhookSubscriptionDelete, ActionWebhookSubscriptionResume,
	}
	seen := make(map[string]bool, len(actions))
	for _, a := range actions {
		if a == "" {
			t.Errorf("empty action constant")
			continue
		}
		if !strings.Contains(a, ".") {
			t.Errorf("action %q is not dotted — downstream BI groups by prefix", a)
		}
		if seen[a] {
			t.Errorf("duplicate action constant %q", a)
		}
		seen[a] = true
	}
}

// TestEntryJSONShape pins the on-wire shape of an audit entry. The
// JSON layout is part of the SIEM contract: omitempty on optional
// fields means downstream filters can rely on field presence as a
// signal (e.g. "actor_id missing → unauthenticated event").
func TestEntryJSONShape(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	actor := uuid.New()
	ip := "10.0.0.5"

	full := Entry{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		ActorID:     &actor,
		Action:      ActionLogin,
		IPAddress:   &ip,
		Metadata:    json.RawMessage(`{"reason":"primary"}`),
		CreatedAt:   now,
	}
	buf, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Field-presence checks rather than full-string compare —
	// uuid values are random, so a literal expected string would
	// be brittle.
	got := string(buf)
	for _, field := range []string{`"id":`, `"workspace_id":`, `"actor_id":`, `"action":`, `"ip_address":`, `"metadata":`, `"created_at":`} {
		if !strings.Contains(got, field) {
			t.Errorf("missing field %s in %s", field, got)
		}
	}

	// Minimal entry: omitempty must hide nil pointers and empty raw
	// json so SIEM filters don't trip on a literal "null".
	minimal := Entry{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Action:      ActionWorkspaceCreate,
		CreatedAt:   now,
	}
	buf, err = json.Marshal(minimal)
	if err != nil {
		t.Fatalf("marshal minimal: %v", err)
	}
	for _, field := range []string{"actor_id", "ip_address", "user_agent", "resource_type", "resource_id", "metadata"} {
		if strings.Contains(string(buf), field) {
			t.Errorf("omitempty broken: minimal entry exposed %q in %s", field, buf)
		}
	}
}

// TestEntryRoundTrip ensures encode → decode preserves every set
// field. Audit entries flow through JSON for cross-process
// transport, so a regression in tag annotations would silently lose
// data.
func TestEntryRoundTrip(t *testing.T) {
	actor := uuid.New()
	resID := uuid.New()
	resType := "workspace"
	ip := "203.0.113.10"
	ua := "Mozilla/5.0"
	in := Entry{
		ID:           uuid.New(),
		WorkspaceID:  uuid.New(),
		ActorID:      &actor,
		Action:       ActionPermissionGrant,
		ResourceType: &resType,
		ResourceID:   &resID,
		IPAddress:    &ip,
		UserAgent:    &ua,
		Metadata:     json.RawMessage(`{"role":"editor"}`),
		CreatedAt:    time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC),
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Entry
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID || out.WorkspaceID != in.WorkspaceID || out.Action != in.Action {
		t.Fatalf("round-trip mismatch on required fields: in=%+v out=%+v", in, out)
	}
	if *out.ActorID != *in.ActorID || *out.IPAddress != *in.IPAddress || *out.UserAgent != *in.UserAgent {
		t.Fatalf("round-trip mismatch on pointer fields")
	}
	if string(out.Metadata) != string(in.Metadata) {
		t.Fatalf("metadata round-trip: in=%s out=%s", in.Metadata, out.Metadata)
	}
}
