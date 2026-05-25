package document

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// FolderLookup is the minimal interface this service needs from the
// folder layer: given a workspace + folder id, return the folder so
// the service can read its EncryptionMode. The full
// folder.Service implements this; tests can supply a fake.
type FolderLookup interface {
	GetByID(ctx context.Context, workspaceID, folderID uuid.UUID) (*folder.Folder, error)
}

// Service is the business-logic layer over Repository + FolderLookup.
// It enforces the capability matrix (collab_mode must be allowed by
// the folder's encryption_mode) and orchestrates compaction.
type Service struct {
	repo    Repository
	folders FolderLookup
}

// NewService returns a Service backed by the given repository and
// folder lookup.
func NewService(repo Repository, folders FolderLookup) *Service {
	return &Service{repo: repo, folders: folders}
}

// CreateInput is the validated user request for creating a document.
// CollabMode may be empty, in which case the service picks the
// richest allowed mode for the folder's encryption_mode.
type CreateInput struct {
	WorkspaceID uuid.UUID
	FolderID    uuid.UUID
	Name        string
	CollabMode  string
	CreatedBy   uuid.UUID
}

// Create validates inputs against folder capability and inserts the
// document. The caller (HTTP handler) is responsible for tenant guard
// (workspace membership), this service trusts the workspaceID is
// authoritative.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Document, *folder.Folder, error) {
	name := strings.TrimSpace(in.Name)
	if !isValidDocumentName(name) {
		return nil, nil, ErrInvalidName
	}
	parent, err := s.folders.GetByID(ctx, in.WorkspaceID, in.FolderID)
	if err != nil {
		return nil, nil, err
	}
	collabMode := in.CollabMode
	if collabMode == "" {
		collabMode = DefaultCollabModeFor(parent.EncryptionMode)
	} else {
		if !IsValidCollabMode(collabMode) {
			return nil, nil, ErrInvalidCollabMode
		}
		if !IsCollabModeAllowed(parent.EncryptionMode, collabMode) {
			return nil, nil, ErrCollabModeNotAllowed
		}
	}

	d := &Document{
		WorkspaceID: in.WorkspaceID,
		FolderID:    in.FolderID,
		Name:        name,
		CollabMode:  collabMode,
		CreatedBy:   in.CreatedBy,
	}
	if err := s.repo.Create(ctx, d); err != nil {
		return nil, nil, err
	}
	return d, parent, nil
}

// GetByID fetches a document along with its parent folder so the
// caller can compute live capability + encryption_mode in one place.
// Returning the folder (not just a derived Capability struct) avoids
// a second round-trip in the HTTP layer when it needs the
// parent.EncryptionMode for the response envelope.
func (s *Service) GetByID(ctx context.Context, workspaceID, documentID uuid.UUID) (*Document, *folder.Folder, error) {
	d, err := s.repo.GetByID(ctx, workspaceID, documentID)
	if err != nil {
		return nil, nil, err
	}
	parent, err := s.folders.GetByID(ctx, workspaceID, d.FolderID)
	if err != nil {
		return nil, nil, fmt.Errorf("lookup document folder: %w", err)
	}
	return d, parent, nil
}

// Rename updates the document name.
func (s *Service) Rename(ctx context.Context, workspaceID, documentID uuid.UUID, name string) (*Document, error) {
	name = strings.TrimSpace(name)
	if !isValidDocumentName(name) {
		return nil, ErrInvalidName
	}
	return s.repo.UpdateName(ctx, workspaceID, documentID, name)
}

// SetCollabMode changes the document's collab_mode. Validates the
// new mode against the folder's current encryption_mode.
func (s *Service) SetCollabMode(ctx context.Context, workspaceID, documentID uuid.UUID, collabMode string) (*Document, error) {
	if !IsValidCollabMode(collabMode) {
		return nil, ErrInvalidCollabMode
	}
	d, err := s.repo.GetByID(ctx, workspaceID, documentID)
	if err != nil {
		return nil, err
	}
	parent, err := s.folders.GetByID(ctx, workspaceID, d.FolderID)
	if err != nil {
		return nil, fmt.Errorf("lookup folder for collab mode change: %w", err)
	}
	// 'disabled' is the only mode a user is never allowed to pick
	// directly. It is set by the service when a folder is migrated
	// to a mode that doesn't allow the document's current collab
	// mode (currently unreachable since folder mode is immutable,
	// but kept defensive against the future "migrate folder" admin
	// path).
	if collabMode == CollabModeDisabled {
		return nil, ErrInvalidCollabMode
	}
	if !IsCollabModeAllowed(parent.EncryptionMode, collabMode) {
		return nil, ErrCollabModeNotAllowed
	}
	return s.repo.UpdateCollabMode(ctx, workspaceID, documentID, collabMode)
}

// Delete soft-deletes the document. Deltas are retained.
func (s *Service) Delete(ctx context.Context, workspaceID, documentID uuid.UUID) error {
	return s.repo.SoftDelete(ctx, workspaceID, documentID)
}

// ListByFolder returns the documents in a folder, most-recently-
// updated first.
func (s *Service) ListByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*Document, error) {
	return s.repo.ListByFolder(ctx, workspaceID, folderID)
}

// AppendDeltaInput is the validated user request for appending a
// single Yjs update to a document.
type AppendDeltaInput struct {
	WorkspaceID  uuid.UUID
	DocumentID   uuid.UUID
	Payload      []byte
	AuthorUserID uuid.UUID
}

// AppendDelta inserts a delta, then conditionally returns a hint
// that compaction is due. The hint is advisory — the WebSocket
// provider (P2b) consumes it to schedule a snapshot fold during a
// quiet moment rather than blocking the live update path.
type AppendDeltaResult struct {
	Delta              *Delta
	CompactionDue      bool
	PendingDeltaCount  int64
}

// AppendDelta validates the payload and inserts the delta. Returns
// the persisted Delta with its server-assigned seq, plus a hint
// indicating whether the count of pending deltas (those above the
// snapshot floor) has crossed CompactionThreshold.
func (s *Service) AppendDelta(ctx context.Context, in AppendDeltaInput) (*AppendDeltaResult, error) {
	if len(in.Payload) == 0 {
		return nil, ErrEmptyPayload
	}
	if len(in.Payload) > MaxDeltaPayloadBytes {
		return nil, ErrPayloadTooLarge
	}
	// We refuse to append to a 'disabled' document — the user has
	// to flip it back to a real collab mode first. This is also a
	// nice early-cut for the case where the WebSocket provider is
	// out of sync with a recent SetCollabMode change.
	d, err := s.repo.GetByID(ctx, in.WorkspaceID, in.DocumentID)
	if err != nil {
		return nil, err
	}
	if d.CollabMode == CollabModeDisabled {
		return nil, fmt.Errorf("%w: document is disabled", ErrCollabModeNotAllowed)
	}

	delta := &Delta{
		DocumentID:   in.DocumentID,
		Payload:      in.Payload,
		AuthorUserID: in.AuthorUserID,
		WorkspaceID:  in.WorkspaceID,
	}
	if err := s.repo.AppendDelta(ctx, delta); err != nil {
		return nil, err
	}

	count, err := s.repo.CountDeltasAfter(ctx, in.WorkspaceID, in.DocumentID, d.YStateSeqFloor)
	if err != nil {
		// Don't fail the append on a counting error — surface the
		// successful delta and let the next caller's count drive
		// the compaction decision.
		return &AppendDeltaResult{Delta: delta}, nil
	}
	return &AppendDeltaResult{
		Delta:             delta,
		CompactionDue:     count >= CompactionThreshold,
		PendingDeltaCount: count,
	}, nil
}

// ListDeltas returns deltas above the supplied cursor.
func (s *Service) ListDeltas(ctx context.Context, workspaceID, documentID uuid.UUID, afterSeq int64, limit int) ([]*Delta, error) {
	return s.repo.ListDeltas(ctx, workspaceID, documentID, afterSeq, limit)
}

// SnapshotResult bundles the document's snapshot + tail-deltas for
// a cold-opening client. Clients restore Y.Doc from y_state then
// apply each tail delta in seq order. Folder is exposed (not just
// the derived Capability) so the HTTP layer can compute both the
// capability AND the encryption_mode label for its response.
type SnapshotResult struct {
	Document   *Document
	Folder     *folder.Folder
	Capability Capability
	TailDeltas []*Delta
}

// Snapshot returns the latest snapshot + every delta with seq above
// y_state_seq_floor. Clients use this as the one-shot
// "open this document" payload.
func (s *Service) Snapshot(ctx context.Context, workspaceID, documentID uuid.UUID) (*SnapshotResult, error) {
	d, parent, err := s.GetByID(ctx, workspaceID, documentID)
	if err != nil {
		return nil, err
	}
	// Cap the tail at MaxSnapshotTailDeltas — if the tail is longer
	// the caller should trigger compaction; for now this is the
	// theoretical upper bound until P2b lands the WS snapshot job.
	deltas, err := s.repo.ListDeltas(ctx, workspaceID, documentID, d.YStateSeqFloor, MaxSnapshotTailDeltas)
	if err != nil {
		return nil, err
	}
	return &SnapshotResult{
		Document:   d,
		Folder:     parent,
		Capability: ResolveCapability(parent.EncryptionMode),
		TailDeltas: deltas,
	}, nil
}

// Compact folds the tail deltas (seq > y_state_seq_floor) into a
// new snapshot. The fold function is supplied by the caller because
// the merge strategy depends on the folder's encryption mode:
//
//   - managed_encrypted: server-side Yjs merge (decrypt, applyUpdate,
//     re-encrypt). The fold callback typically calls into the Yjs
//     bindings via a CGo or out-of-process bridge.
//
//   - strict_zk: opaque concatenation (server can't decrypt). The
//     fold callback returns the deltas concatenated with a length-
//     prefix framing so the client can split them on the other end.
//
// Both paths produce a new y_state and y_state_vector that are
// persisted via ReplaceSnapshot. The fold callback receives the
// CURRENT snapshot bytes + ordered tail deltas and returns the new
// snapshot bytes + state vector + the seq of the last delta folded.
type FoldFunc func(currentState, currentStateVector []byte, tail []*Delta) (newState, newStateVector []byte, upToSeq int64, err error)

// Compact runs the fold and atomically swaps the snapshot. Returns
// the updated Document. Idempotent: re-running with an empty tail
// is a no-op (returns the existing document unchanged).
func (s *Service) Compact(ctx context.Context, workspaceID, documentID uuid.UUID, fold FoldFunc) (*Document, error) {
	d, err := s.repo.GetByID(ctx, workspaceID, documentID)
	if err != nil {
		return nil, err
	}
	tail, err := s.repo.ListDeltas(ctx, workspaceID, documentID, d.YStateSeqFloor, MaxSnapshotTailDeltas)
	if err != nil {
		return nil, err
	}
	if len(tail) == 0 {
		return d, nil
	}
	newState, newStateVector, upToSeq, err := fold(d.YState, d.YStateVector, tail)
	if err != nil {
		return nil, fmt.Errorf("compaction fold: %w", err)
	}
	if upToSeq <= d.YStateSeqFloor {
		// Fold callback returned a stale upToSeq. Refuse to
		// regress the snapshot floor; raise a typed error so the
		// caller can log + retry.
		return nil, errors.New("compaction fold returned non-progressing upToSeq")
	}
	return s.repo.ReplaceSnapshot(ctx, workspaceID, documentID, newState, newStateVector, upToSeq)
}

// MaxSnapshotTailDeltas caps the number of tail deltas returned by
// Snapshot. The compaction threshold is 64; a cap of 10x gives
// headroom for periods where compaction is paused (e.g. during
// connectivity outages) without an unbounded tail. Beyond this cap
// the snapshot endpoint truncates; clients should re-issue
// ListDeltas with the highest returned seq as the cursor.
const MaxSnapshotTailDeltas = 640

// isValidDocumentName mirrors the Postgres CHECK on documents.name:
// non-empty and <= MaxNameBytes. We also reject '/' so future
// path-based features can't be confused by a name containing the
// separator.
func isValidDocumentName(name string) bool {
	if name == "" || len(name) > MaxNameBytes {
		return false
	}
	if strings.ContainsRune(name, '/') {
		return false
	}
	return true
}
