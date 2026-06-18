package drive

import (
	"context"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// WebhookEventPublisher is the narrow interface the drive handler
// depends on for outbound-webhook emission. Defined here (in the
// consumer) rather than re-using *webhooks.Publisher directly so
// (a) tests can inject a fake publisher without standing up a real
// NATS/JetStream connection, and (b) the handler's surface area on
// the publisher is documented to exactly the two emit-helpers it
// uses. The concrete *webhooks.Publisher in internal/webhooks
// satisfies this interface, so production wiring is unchanged.
type WebhookEventPublisher interface {
	PublishFileEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.FileEventData) error
	PublishPermissionEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data webhooks.PermissionEventData) error
}

// webhookActorID extracts the authenticated user's id from ctx and
// returns it as a pointer for embedding in an Event envelope. Returns
// nil when no user is present (e.g. background tasks / cron),
// matching the actor_id schema in the Event struct which is JSON-
// omitted when nil.
func webhookActorID(ctx context.Context) *uuid.UUID {
	uid, ok := middleware.UserIDFromContext(ctx)
	if !ok {
		return nil
	}
	return &uid
}

// publishWebhookFileEvent is the centralised emit-helper for the
// file.* event family. Factored out so future events (e.g.
// file.versioned, file.checksum_failed) drop into one place rather
// than open-coding the envelope construction at every call site.
// Publish failures are logged at error level but do NOT propagate to
// the caller — webhook emission is a side-effect, not part of the
// success contract of the parent HTTP handler.
func (h *Handler) publishWebhookFileEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, f *file.File, versionID uuid.UUID, sizeBytes int64) {
	if h.webhooks == nil || f == nil {
		return
	}
	// Pass nil for VersionID / FolderID when the value is zero so
	// the JSON output omits the field entirely (FileEventData now
	// uses pointers — see the docblock there for why uuid.UUID's
	// fixed-size [16]byte shape defeats `omitempty`).
	var versionPtr *uuid.UUID
	if versionID != uuid.Nil {
		v := versionID
		versionPtr = &v
	}
	var folderPtr *uuid.UUID
	if f.FolderID != uuid.Nil {
		fid := f.FolderID
		folderPtr = &fid
	}
	if err := h.webhooks.PublishFileEvent(ctx, t, workspaceID, webhookActorID(ctx), webhooks.FileEventData{
		FileID:    f.ID,
		VersionID: versionPtr,
		FolderID:  folderPtr,
		Name:      f.Name,
		MimeType:  f.MimeType,
		SizeBytes: sizeBytes,
	}); err != nil {
		logging.FromContext(ctx).Error("drive publish webhook file event failed",
			"event_type", string(t),
			"file_id", f.ID,
			"workspace_id", workspaceID,
			"err", err,
		)
	}
}

// publishWebhookPermissionEvent emits a permission.* event. Same
// nil-safe / log-but-don't-fail contract as publishWebhookFileEvent.
func (h *Handler) publishWebhookPermissionEvent(ctx context.Context, t webhooks.EventType, workspaceID uuid.UUID, resourceType string, resourceID, granteeID uuid.UUID, role string) {
	if h.webhooks == nil {
		return
	}
	// Zero-valued grantee_id (e.g. guest-link grant where no user
	// account exists yet) becomes nil so the JSON field is omitted
	// rather than serialised as the zero UUID.
	var granteePtr *uuid.UUID
	if granteeID != uuid.Nil {
		g := granteeID
		granteePtr = &g
	}
	if err := h.webhooks.PublishPermissionEvent(ctx, t, workspaceID, webhookActorID(ctx), webhooks.PermissionEventData{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		GranteeID:    granteePtr,
		Role:         role,
	}); err != nil {
		logging.FromContext(ctx).Error("drive publish webhook permission event failed",
			"event_type", string(t),
			"resource_id", resourceID,
			"workspace_id", workspaceID,
			"err", err,
		)
	}
}

// snapshotFilesForFolderSubtreeDelete is the first half of the
// two-phase folder-cascade webhook pattern: it returns every
// non-deleted file under the given folder (including descendants),
// so the caller can later emit a file.deleted webhook per snapshot
// via emitWebhookFileDeletedBatch AFTER folders.Delete succeeds.
//
// Callers MUST invoke this BEFORE calling folders.Delete — once the
// recursive folder soft-delete cascades to files.deleted_at the
// rows would no longer match the snapshotting query's deleted_at
// IS NULL filter, leaving subscribers in the dark about cascaded
// files. The split into snapshot + emit (instead of one
// publish-everything helper) is deliberate so the caller can abort
// emission if folders.Delete itself fails — we never want
// subscribers to see a file.deleted event for a file that wasn't
// actually deleted on the server.
//
// Closes the asymmetry between single-file deletes (which already
// emit file.deleted) and folder-cascade deletes (which used to emit
// nothing for the contained files). The event catalog only defines
// file.deleted (there's no folder.deleted yet) so subscribers who
// register for file.deleted to drive "files in our workspace have
// been removed" workflows now see the same stream regardless of
// whether the file was deleted individually or via folder cascade.
//
// Snapshot errors are logged but do NOT block the folder delete —
// webhook emission is a side-effect, not part of the success
// contract of the parent HTTP handler. The same TOCTOU window that
// exists for single-file emit (snapshot -> delete, microseconds
// apart, no transaction) applies here and is accepted as part of
// the documented at-least-once delivery contract: subscribers must
// dedupe on X-ZkDrive-Event-Id.
func (h *Handler) snapshotFilesForFolderSubtreeDelete(ctx context.Context, workspaceID, folderID uuid.UUID) []*file.File {
	if h.webhooks == nil {
		return nil
	}
	snaps, err := h.files.ListInFolderSubtree(ctx, workspaceID, folderID)
	if err != nil {
		logging.FromContext(ctx).Error("drive failed to snapshot folder subtree for file.deleted webhook cascade",
			"folder_id", folderID,
			"workspace_id", workspaceID,
			"err", err,
		)
		return nil
	}
	return snaps
}

// emitWebhookFileDeletedBatch is the second half of the two-phase
// folder-cascade webhook pattern: emit a file.deleted webhook per
// snapshot returned by snapshotFilesForFolderSubtreeDelete, AFTER
// folders.Delete has succeeded. Split into a separate call so
// handlers can interleave the actual folder soft-delete between
// the snapshot and the emission, giving callers the freedom to
// abort emission if the delete itself fails (preventing phantom
// events for files that weren't actually deleted).
func (h *Handler) emitWebhookFileDeletedBatch(ctx context.Context, workspaceID uuid.UUID, snaps []*file.File) {
	if h.webhooks == nil || len(snaps) == 0 {
		return
	}
	for _, f := range snaps {
		h.publishWebhookFileEvent(ctx, webhooks.EventFileDeleted, workspaceID, f, uuid.Nil, f.SizeBytes)
	}
}

// snapshotDocumentsForFolderSubtreeDelete is the document counterpart
// to snapshotFilesForFolderSubtreeDelete: it returns every non-deleted
// collab document under the given folder (including descendants) so
// the caller can later emit a document.delete activity + changefeed
// event per snapshot AFTER folders.Delete has cascaded its
// deleted_at update to the documents table.
//
// Same two-phase contract as the file path: callers MUST invoke this
// BEFORE folders.Delete. Once the recursive folder soft-delete
// cascades to documents.deleted_at the rows would no longer match
// the snapshotting query's deleted_at IS NULL filter, leaving the
// changefeed (and any subscribed desktop sync clients) unaware that
// the documents were removed. The catalogue would then keep stale
// 'up_to_date' rows pointing at documents that 404 on next open.
//
// Symmetric with the per-document ActionDocumentDelete path so
// subscribers see document.delete events regardless of whether the
// document was deleted individually or via folder cascade. Returns
// nil + logs on error so a snapshot failure never blocks the parent
// folder delete — the change feed is best-effort relative to the
// durable activity log; a stuck snapshot must not strand the
// folder-delete HTTP path.
//
// When the documents service is not wired (collab feature disabled
// for this deployment) we short-circuit to nil. SoftDeleteSubtree
// still cascades to the documents table for data-integrity, but no
// events fire because there is no service to emit through.
func (h *Handler) snapshotDocumentsForFolderSubtreeDelete(ctx context.Context, workspaceID, folderID uuid.UUID) []*document.Document {
	if h.documents == nil {
		return nil
	}
	snaps, err := h.documents.ListByFolderSubtree(ctx, workspaceID, folderID)
	if err != nil {
		logging.FromContext(ctx).Error("drive failed to snapshot folder subtree for document.delete cascade",
			"folder_id", folderID,
			"workspace_id", workspaceID,
			"err", err,
		)
		return nil
	}
	return snaps
}

// emitFolderCascadeDocumentDeletes is the single-handler half of
// the two-phase document-cascade pattern. Pair with
// snapshotDocumentsForFolderSubtreeDelete: snapshot before
// folders.Delete, emit after. Delegates to the same batch builder
// used by the bulk handler so per-document changefeed writes flush
// in ONE Postgres round-trip (multi-row INSERT) instead of N
// sequential round-trips. Mirrors the same batching pattern used
// for bulk file/folder mutations: deleting a project folder
// with 50 documents previously cost 50 changefeed INSERTs; now it
// costs 1.
//
// Activity log entries are still per-item via logActivityOnly
// inside the batch builder — that path is already async (LogAction
// enqueues onto a buffered channel) so per-item dispatch is cheap.
// Only the changefeed mirror gets the batched flush.
//
// Bulk callers (BulkDelete) call emitFolderCascadeDocumentDeletes-
// Batch directly so they can accumulate the changeInput entries
// across multiple folders into ONE outer batchRecordChanges call —
// see api/drive/bulk.go.
func (h *Handler) emitFolderCascadeDocumentDeletes(ctx context.Context, snaps []*document.Document) {
	h.batchRecordChanges(ctx, h.emitFolderCascadeDocumentDeletesBatch(ctx, snaps))
}

// emitFolderCascadeDocumentDeletesBatch is the bulk-handler variant.
// Returns the per-document changeInput entries the caller appends
// to its outer accumulator so a single batchRecordChanges flushes
// all cascaded document deletes in one Postgres round-trip. The
// activity log is still per-item (LogAction is buffered + async,
// so per-item dispatch is already cheap there).
//
// The same metadata map (folder_id, name) is supplied to BOTH the
// activity-log entry and the changeInput so sync clients see the
// same payload regardless of whether the document was cascade-
// deleted via /folders/{id} (single-handler path, logActivity →
// recordChange mirrors metadata into the changefeed) or
// /bulk/delete (this path). A subscriber filtering on
// kind=document op=delete must receive parent_id + name in either
// case so it can drop the local catalogue entry from its UI
// without a follow-up GET that would 404.
func (h *Handler) emitFolderCascadeDocumentDeletesBatch(ctx context.Context, snaps []*document.Document) []changeInput {
	out := make([]changeInput, 0, len(snaps))
	for _, d := range snaps {
		md := map[string]any{
			"folder_id": d.FolderID,
			"name":      d.Name,
		}
		h.logActivityOnly(ctx, activity.ActionDocumentDelete, resourceTypeDocument, d.ID, md)
		out = append(out, changeInput{
			Action:       activity.ActionDocumentDelete,
			ResourceType: resourceTypeDocument,
			ResourceID:   d.ID,
			Metadata:     md,
		})
	}
	return out
}

// NOTE: member.* webhook emission lives in api/admin (not here)
// because the invite / deactivate handlers are mounted there. The
// admin handler has its own publishMemberEvent helper that mirrors
// the nil-safe contract above.
