package drive

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
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
		log := logging.FromContext(ctx)
		if log == nil {
			log = slog.Default()
		}
		log.Error("drive publish webhook file event failed",
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
		log := logging.FromContext(ctx)
		if log == nil {
			log = slog.Default()
		}
		log.Error("drive publish webhook permission event failed",
			"event_type", string(t),
			"resource_id", resourceID,
			"workspace_id", workspaceID,
			"err", err,
		)
	}
}

// NOTE: member.* webhook emission lives in api/admin (not here)
// because the invite / deactivate handlers are mounted there. The
// admin handler has its own publishMemberEvent helper that mirrors
// the nil-safe contract above.
