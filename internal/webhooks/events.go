// Package webhooks implements outbound webhook subscriptions for
// workspace events (WS-24).
//
// # The product gap this closes
//
// Internal automation, customer integrations, Zapier / n8n flows,
// and KChat (the team chat product the README documents as ZK Drive's
// primary B2B consumer) all want low-latency notifications when
// state changes in a workspace — a new file is uploaded, a permission
// is granted, a member joins. Without outbound webhooks every
// integration has to poll, which is bandwidth-inefficient AND can't
// react with low latency.
//
// # Architecture
//
// 1. Domain handlers (API server, worker) call Publisher.Publish to
//    emit a typed Event. Publish is a no-op when the Publisher is nil
//    so callers can wire it unconditionally during startup.
//
// 2. The Publisher serialises the Event as JSON and sends it on the
//    "webhook.events" NATS JetStream subject. JetStream gives us
//    durable queueing and the OTel context-propagation contract the
//    rest of the codebase already uses (see internal/jobs).
//
// 3. The DeliveryWorker (run from cmd/worker) is a durable JetStream
//    consumer. For each message it: (a) fetches every active
//    subscription matching (workspace_id, event_type), (b) signs the
//    payload with each subscription's secret, (c) POSTs to the URL
//    with a 30s timeout, (d) records the attempt in webhook_deliveries.
//    On non-2xx / network failure it nacks with a delay so JetStream
//    re-delivers up to MaxAttempts times with exponential backoff.
//
// 4. After MaxAttempts consecutive non-success outcomes for ANY event
//    on a subscription, the worker auto-pauses the subscription
//    (active=false, auto_paused_at=now). Admins resume it from the
//    UI after fixing their endpoint.
//
// # Why JetStream rather than a per-row "next_attempt_at" worker
//
// The "poll a table for ready rows" pattern works but adds another
// hot Postgres query path and a polling cadence to tune. JetStream
// is already deployed for the preview / scan / index jobs, supports
// per-message delivery delay (Ack().AckProgress + Nak with delay), and
// gives us the OTel propagation contract for free. The webhook_deliveries
// table stays append-only — pure history, no scheduling state.
//
// # Security
//
// Three layers stack on top of each other:
//
//  1. Admin auth: only workspace admins can create/list/delete
//     subscriptions (WS-4 enforcement at the route).
//  2. URL validation: SSRF defense rejects RFC1918, link-local,
//     loopback, multicast, and cloud-metadata IPs at create time AND
//     re-validates at every delivery (DNS rebinding defense).
//  3. Signature: HMAC-SHA256 over `t=<unix>.<body>`. Header
//     "X-ZkDrive-Signature: t=1700000000,v1=<hex>" mirrors Stripe's
//     scheme, so consumers can reuse familiar verification snippets.
//     5-minute timestamp tolerance prevents replay.
package webhooks

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EventType is the dotted-namespace string identifying which class
// of state change occurred. Stored as TEXT in the database (not an
// ENUM) so adding a new event type is a code-only change with no
// migration. The full set is kept here as the source of truth; the
// API layer's "list available event types" endpoint reads from this
// constants list so the catalog stays in sync.
type EventType string

const (
	// EventFileUploadConfirmed fires when the server-side "confirm
	// upload" handshake completes (the client has uploaded bytes to
	// zk-object-fabric and the metadata row is durable).
	EventFileUploadConfirmed EventType = "file.upload.confirmed"

	// EventFileDeleted fires when a file is moved to trash. (Hard
	// delete after the retention window does not fire — by then the
	// admin already knows the file is going away.)
	EventFileDeleted EventType = "file.deleted"

	// EventFileRestored fires when a trashed file is recovered.
	EventFileRestored EventType = "file.restored"

	// EventPermissionGranted fires when a file or folder is shared
	// with a user, group, or guest (anything that creates a new
	// permission row).
	EventPermissionGranted EventType = "permission.granted"

	// EventPermissionRevoked fires when a permission row is removed
	// or expired.
	EventPermissionRevoked EventType = "permission.revoked"

	// EventMemberJoined fires when a user is added to a workspace
	// (via invitation acceptance or admin add).
	EventMemberJoined EventType = "member.joined"

	// EventMemberRemoved fires when a user is removed from a
	// workspace.
	EventMemberRemoved EventType = "member.removed"
)

// AllEventTypes returns every event type the system currently knows
// how to publish. The API "GET /api/webhooks/event-types" endpoint
// serialises this slice so the UI's create-subscription form stays in
// sync with the server's truth without a separate config file.
func AllEventTypes() []EventType {
	return []EventType{
		EventFileUploadConfirmed,
		EventFileDeleted,
		EventFileRestored,
		EventPermissionGranted,
		EventPermissionRevoked,
		EventMemberJoined,
		EventMemberRemoved,
	}
}

// IsValidEventType reports whether s names an event type the system
// publishes. Used by the subscription-create endpoint to reject
// typos like "files.upload.confirmed" (plural) before they make it
// into the database where they would silently never fire.
func IsValidEventType(s string) bool {
	for _, t := range AllEventTypes() {
		if string(t) == s {
			return true
		}
	}
	return false
}

// Event is the envelope every subscriber receives. The Data field is
// the event-type-specific payload (a FileEventData, MemberEventData,
// etc.); subscribers should decode it according to Type. Keeping the
// envelope flat and self-describing means a single signature-verify
// snippet works for every event type.
type Event struct {
	// ID is the idempotency key. Subscribers MUST dedupe on this
	// value because at-least-once delivery is the JetStream
	// contract: a delivery that succeeds but whose ack is lost will
	// be re-delivered, producing the same ID twice.
	ID uuid.UUID `json:"id"`

	// Type names the kind of state change. Subscribers usually
	// branch on this field before decoding Data.
	Type EventType `json:"type"`

	// WorkspaceID is the tenant the event belongs to. Subscribers
	// only ever receive events for their own workspace (the
	// publisher fan-out is workspace-scoped), but we include the
	// field explicitly so a multi-workspace consumer can dispatch
	// without inferring it from URL or header context.
	WorkspaceID uuid.UUID `json:"workspace_id"`

	// CreatedAt is the moment the event was published, in UTC.
	// Subscribers can compare to "now" to detect replay / clock
	// skew, though the X-ZkDrive-Signature header timestamp is the
	// primary anti-replay defense.
	CreatedAt time.Time `json:"created_at"`

	// ActorID identifies the user whose action triggered the event,
	// when there is one. NULL for system-emitted events (e.g.,
	// retention sweep deleting a file).
	ActorID *uuid.UUID `json:"actor_id,omitempty"`

	// Data is the event-type-specific payload. Encoded as json.RawMessage
	// so the envelope can be parsed once and Data decoded later.
	Data json.RawMessage `json:"data"`
}

// FileEventData is the Data payload shape for every event in the
// "file.*" namespace. Carries the minimal set of identifiers and
// labels a subscriber typically needs to route the event without an
// extra round-trip; richer details (full path, ACL, version history)
// are re-fetched via the regular drive API.
type FileEventData struct {
	FileID    uuid.UUID `json:"file_id"`
	VersionID uuid.UUID `json:"version_id,omitempty"`
	FolderID  uuid.UUID `json:"folder_id,omitempty"`
	Name      string    `json:"name"`
	MimeType  string    `json:"mime_type,omitempty"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
}

// PermissionEventData is the Data payload shape for permission.*
// events. Identifies WHAT was shared, WITH WHOM, and AT WHAT level.
type PermissionEventData struct {
	ResourceType string    `json:"resource_type"` // "file" or "folder"
	ResourceID   uuid.UUID `json:"resource_id"`
	GranteeID    uuid.UUID `json:"grantee_id,omitempty"`
	// GranteeEmail is set for guest-link / external-email grants when
	// no user account exists; for user-level grants it is empty and
	// GranteeID carries the user UUID.
	GranteeEmail string `json:"grantee_email,omitempty"`
	Role         string `json:"role"` // "viewer" | "editor" | "admin"
}

// MemberEventData is the Data payload shape for member.* events.
type MemberEventData struct {
	UserID uuid.UUID `json:"user_id"`
	Email  string    `json:"email"`
	Role   string    `json:"role"` // "admin" | "member" | "guest"
}

// NewEvent builds an Event with a fresh ID and a CreatedAt of now.
// The caller marshals data into json.RawMessage; we accept it pre-
// marshalled so the publisher doesn't need to know about every event
// type's payload shape.
func NewEvent(t EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data json.RawMessage) Event {
	return Event{
		ID:          uuid.New(),
		Type:        t,
		WorkspaceID: workspaceID,
		CreatedAt:   time.Now().UTC(),
		ActorID:     actorID,
		Data:        data,
	}
}

// ErrEventTypeUnknown is returned by validation when the supplied
// string is not a member of AllEventTypes(). Exported so callers can
// distinguish "user typo'd the event type" from "something else broke".
var ErrEventTypeUnknown = errors.New("webhooks: unknown event type")

// NormaliseEventType lowercases and trims user input before
// comparing it against AllEventTypes. Operators paste event types
// from documentation, so accepting "File.Upload.Confirmed" or
// "  file.upload.confirmed  " is just kindness.
func NormaliseEventType(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
