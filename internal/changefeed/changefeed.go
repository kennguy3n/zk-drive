// Package changefeed exposes a durable, monotonically-ordered log of
// state-mutating operations on a workspace's files, folders, and
// permissions. Desktop sync clients consume the feed in two
// complementary modes:
//
//   - "Catch-up" via cursor-paged REST: GET /api/v1/changes?since=N
//     returns every mutation after sequence N for the caller's
//     workspace, in ascending order. Used on reconnect.
//   - "Live" via WebSocket: connected clients receive a JSON envelope
//     for every mutation in their workspace as it lands, fanned out
//     by Service.broadcast.
//
// The package is intentionally lower-level than activity:
// activity.Service is a fire-and-forget telemetry pipeline (writes
// are async, the buffer drops on overflow), whereas changefeed
// writes are synchronous so a sync client never falls out of sync
// because a buffer was full. The two stores are kept separate so
// that telemetry retention / sampling policies don't interact with
// sync correctness.
//
// The schema and cursor model live in
// migrations/029_change_log.up.sql; the doc comment there is the
// canonical reference.
package changefeed

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Kind names the resource family the mutation applies to. These
// strings are persisted in change_log.kind (CHECK-constrained) and
// emitted to clients verbatim, so adding a new value requires both
// a migration update and a sync-client SDK update — keep this list
// short.
//
// ADDING A NEW Kind: the permission cache hook in service.go
// (shouldBustForMutation) defaults to no-bust for unknown
// kinds. If your new Kind affects access resolution you MUST
// update both shouldBustForMutation AND the audit ledger
// knownKindOpBustDecisions in service_test.go. The
// expectedKindCount sentinel in service_test.go trips CI when
// a new Kind is added without updating the ledger — see the
// doc comment on shouldBustForMutation for the full workflow.
const (
	KindFile       = "file"
	KindFolder     = "folder"
	KindPermission = "permission"
	// KindDocument names collab editor (P2) document mutations.
	// The change_log.kind CHECK accepts this value from migration
	// 031 onward — older deployments that haven't yet applied 031
	// will reject document mutations at the database layer.
	KindDocument = "document"
)

// Op names the state transition. delete is final (no resurrection
// path today); rename and move are kept separate so clients can
// optimise the rename-in-place fast path without re-fetching the
// parent's listing.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpRename = "rename"
	OpMove   = "move"
	OpDelete = "delete"
)

// Mutation is one persisted change_log row. It is also the live
// JSON envelope pushed over WebSocket — both routes share the same
// shape so a sync client's reconciliation code is identical between
// catch-up and live modes.
//
// JSON tag note: ParentID, Name, and Metadata are omitempty so that
// per-event minimal envelopes do not carry "parent_id": null /
// "name": "" / "metadata": null lines for ops that don't supply
// them (notably delete, where the parent / name have already been
// erased from the live tree). The Sequence is always present
// because it is the cursor.
type Mutation struct {
	Sequence    int64           `json:"sequence"`
	WorkspaceID uuid.UUID       `json:"workspace_id"`
	ActorID     *uuid.UUID      `json:"actor_id,omitempty"`
	Kind        string          `json:"kind"`
	Op          string          `json:"op"`
	ResourceID  uuid.UUID       `json:"resource_id"`
	ParentID    *uuid.UUID      `json:"parent_id,omitempty"`
	Name        string          `json:"name,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

// Page is the response shape for cursor-paged catch-up reads. Cursor
// is the sequence of the last row in Mutations (or the supplied
// `since` value when Mutations is empty) so clients can resume
// trivially. HasMore is true when a follow-up call may return more
// rows immediately — clients should keep paging until HasMore is
// false before declaring they are caught up.
type Page struct {
	Mutations []Mutation `json:"mutations"`
	Cursor    int64      `json:"cursor"`
	HasMore   bool       `json:"has_more"`
}
