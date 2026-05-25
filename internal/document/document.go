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

// MaxNameBytes is the application-side cap on documents.name.
// NOTE: the Postgres CHECK constraint at migrations/030 uses
// `length(name) <= 512`, which counts CHARACTERS, while this
// constant is enforced in Go via `len(name)`, which counts BYTES.
// They agree exactly for ASCII names; for multi-byte UTF-8 (e.g.
// emoji) Go is stricter and rejects before the DB ever sees the
// row, so the Postgres constraint can never trigger from a valid
// API path. Treat this as the effective name limit; the DB
// constraint is defence-in-depth against direct SQL access.
const MaxNameBytes = 512

// MaxDeltaPageLimit caps the per-page result count for the public
// delta-list endpoint. Chosen lower than the snapshot tail cap
// (MaxSnapshotTailDeltas = 640) so a client paging deltas always
// gets the same effective limit it asks for — the HTTP handler
// clamps to this value and echoes the clamped result back, so the
// `len(deltas) < limit` paging idiom remains reliable.
//
// INVARIANT: MaxDeltaPageLimit + 1 < MaxDeltaListLimit. The HTTP
// handler probes one row beyond `limit` to populate `has_more`, so
// the repo's internal cap must be strictly larger than the largest
// probe to avoid silently truncating the probe (which would make
// has_more=false incorrectly when more rows exist). Enforced by
// TestDeltaListLimits_HandlerProbeFitsInRepoCap.
const MaxDeltaPageLimit = 500

// MaxDeltaListLimit is the repository's hard cap on a single
// ListDeltas call. Exported so callers (and tests) can reason
// about the worst-case query bound directly instead of inferring it
// from a private constant. Sized comfortably above
// MaxDeltaPageLimit + 1 (the handler's `has_more` probe) and above
// MaxSnapshotTailDeltas (the snapshot bundle tail size) so neither
// caller's intent is silently truncated by the repo's defensive cap.
const MaxDeltaListLimit = 1000

// MaxDocumentsPerFolder caps the per-folder list result. Documents
// per folder are bounded in normal UX (the document panel paginates
// at ~100 entries) but the repository enforces a hard cap so a
// pathological folder can't slow-list the whole table.
const MaxDocumentsPerFolder = 1000

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
	// ErrSnapshotVersionConflict is returned by ReplaceSnapshot when the
	// caller's expected snapshot_version no longer matches the row's
	// current snapshot_version, meaning a concurrent Compact landed
	// between the caller's read of the document + tail and its attempt
	// to write the fold. Callers should re-read via GetSnapshotBundle
	// and retry the fold from scratch (the rows the previous fold
	// folded have already been trimmed by the winning Compact, so
	// re-running on the fresh state cannot regress the snapshot).
	ErrSnapshotVersionConflict = errors.New("snapshot version conflict; concurrent compaction landed")
)
