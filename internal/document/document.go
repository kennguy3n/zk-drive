// Package document implements the persistence + service layer for
// ZK Drive's collaborative editor (P2). Documents live inside
// folders and inherit their parent folder's encryption mode as the
// privacy boundary — this package never stores its own encryption
// mode column. See migrations/029_collab_documents.up.sql for the
// schema rationale and ARCHITECTURE.md §11 for the end-to-end
// collab editor design.
package document

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Collab modes recognised by the document layer. Markdown is the
// default for any folder because it works under every privacy
// boundary; Rich and RichPresence are only allowed in managed-
// encrypted folders (see capability.go for the policy). Disabled
// is a tombstone state used when a document's folder mode changes
// out from under it and the previous collab mode is no longer
// allowed.
const (
	CollabModeMarkdown     = "markdown"
	CollabModeRich         = "rich"
	CollabModeRichPresence = "rich_presence"
	CollabModeDisabled     = "disabled"
)

// allCollabModes is the canonical list of valid collab_mode values.
// Kept in sync with the CHECK constraint on documents.collab_mode in
// migrations/029_collab_documents.up.sql. The
// `TestCollabMode_ExhaustiveCheckConstraint` test verifies the two
// lists match.
var allCollabModes = []string{
	CollabModeMarkdown,
	CollabModeRich,
	CollabModeRichPresence,
	CollabModeDisabled,
}

// IsValidCollabMode reports whether m is one of the recognised
// collab modes. An empty string is invalid; callers should resolve
// the default via DefaultCollabModeFor before calling this.
func IsValidCollabMode(m string) bool {
	for _, v := range allCollabModes {
		if v == m {
			return true
		}
	}
	return false
}

// AllCollabModes returns a copy of the canonical mode list. Used by
// the exhaustiveness test and by the OpenAPI generator (future).
func AllCollabModes() []string {
	out := make([]string, len(allCollabModes))
	copy(out, allCollabModes)
	return out
}

// Document is a row in the documents table. The encryption mode is
// NOT persisted here — it is derived live from the parent folder
// via internal/folder.GetByID + capability.go's resolver.
type Document struct {
	ID              uuid.UUID  `json:"id"`
	WorkspaceID     uuid.UUID  `json:"workspace_id"`
	FolderID        uuid.UUID  `json:"folder_id"`
	Name            string     `json:"name"`
	CollabMode      string     `json:"collab_mode"`
	YState          []byte     `json:"-"`
	YStateVector    []byte     `json:"-"`
	YStateSeqFloor  int64      `json:"y_state_seq_floor"`
	SnapshotVersion int64      `json:"snapshot_version"`
	CreatedBy       uuid.UUID  `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
}

// Delta is one Yjs update appended to a document. The payload is
// opaque to this layer — it is either an encrypted-wrapped-by-server
// blob (managed_encrypted folders) or an end-to-end-encrypted blob
// (strict_zk folders); the capability resolver decides which.
type Delta struct {
	DocumentID   uuid.UUID `json:"document_id"`
	Seq          int64     `json:"seq"`
	Payload      []byte    `json:"-"`
	AuthorUserID uuid.UUID `json:"author_user_id"`
	CreatedAt    time.Time `json:"created_at"`
	WorkspaceID  uuid.UUID `json:"workspace_id"`
}

// CompactionThreshold is the number of pending deltas above which
// the service will fold them into the snapshot on the next write
// path. 64 is a compromise between (a) keeping the delta tail small
// enough that a cold-opening client can replay it cheaply, and (b)
// not running the (potentially merge-expensive) compaction job on
// every other write. The threshold is exported so the WebSocket
// provider in P2b can tune it for its own buffering strategy if
// necessary.
const CompactionThreshold = 64

// MaxDeltaPayloadBytes mirrors the CHECK constraint on
// document_deltas.payload (1 MiB). A single Yjs update should be
// far below this in practice — large updates suggest the client
// failed to incrementally flush. The service rejects oversized
// payloads with ErrPayloadTooLarge before the INSERT to surface a
// clean 4xx rather than a Postgres constraint violation.
const MaxDeltaPayloadBytes = 1 << 20 // 1 MiB

// MaxNameBytes mirrors the CHECK constraint on documents.name.
const MaxNameBytes = 512

// Sentinel errors so callers can map to HTTP status codes without
// touching repository internals.
var (
	ErrNotFound           = errors.New("document not found")
	ErrInvalidName        = errors.New("invalid document name")
	ErrInvalidCollabMode  = errors.New("invalid collab mode")
	ErrCollabModeNotAllowed = errors.New("collab mode not allowed by folder encryption mode")
	ErrPayloadTooLarge    = errors.New("delta payload too large")
	ErrEmptyPayload       = errors.New("delta payload empty")
	ErrSeqConflict        = errors.New("delta seq conflict; concurrent writer")
)
