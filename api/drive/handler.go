// Package drive serves the workspace / folder / file HTTP API. The
// handler surface is large (~50 endpoints across workspaces, folders,
// files, uploads, permissions, sharing, client rooms, search,
// notifications, previews, tags and activity) so methods are split
// into topical files within this package — see workspace.go,
// folder.go, file.go, upload.go, permission.go, sharing.go,
// client_room.go, search.go, notification.go, preview.go, tags.go,
// activity.go, bulk.go, and helpers.go. All methods belong to the
// single *Handler defined here so the routing surface stays
// internally consistent and dependency wiring lives in one place.
package drive

import (
	"context"

	"net/http"

	"github.com/kennguy3n/zk-drive/internal/logging"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/changefeed"
	"github.com/kennguy3n/zk-drive/internal/collab"
	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/email"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/search"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// Handler serves workspace / folder / file HTTP endpoints.
//
// storage is optional: when nil, the upload-url / confirm-upload /
// download-url endpoints respond with 501 Not Implemented so the server
// can still serve metadata-only APIs without a zk-object-fabric gateway
// configured.
type Handler struct {
	pool           *pgxpool.Pool
	workspaces     *workspace.Service
	folders        *folder.Service
	files          *file.Service
	documents      *document.Service
	users          *user.Service
	storage        *storage.Client
	storageFactory *storage.ClientFactory
	permissions    *permission.Service
	activity       *activity.Service
	changefeed     *changefeed.Service
	sharing        *sharing.Service
	search         *search.Service
	clientRooms    *sharing.ClientRoomService
	jobs           *jobs.Publisher
	notifications  *notification.Service
	email          *email.Service
	previews       preview.Repository
	audit          *audit.Service
	billing        *billing.Service
	webhooks       WebhookEventPublisher
	collab         *collab.DocumentHub
}

// NewHandler constructs a Handler from the underlying services. The pool is
// used to run multi-step writes (e.g. CreateWorkspace) atomically. Pass a
// non-nil storage client to enable presigned URL generation against a
// zk-object-fabric gateway; pass nil to run in metadata-only mode. The
// permission and activity services are optional: when nil the corresponding
// endpoints are disabled and activity events are silently dropped, which
// lets legacy tests wire only the metadata plane.
func NewHandler(
	pool *pgxpool.Pool,
	ws *workspace.Service,
	fs *folder.Service,
	fl *file.Service,
	us *user.Service,
	st *storage.Client,
	perms *permission.Service,
	act *activity.Service,
) *Handler {
	return &Handler{
		pool:        pool,
		workspaces:  ws,
		folders:     fs,
		files:       fl,
		users:       us,
		storage:     st,
		permissions: perms,
		activity:    act,
	}
}

// WithBilling wires the billing service. When non-nil the handler
// enforces storage / bandwidth quotas and records usage events. A
// nil service short-circuits all checks to "allow" — useful for
// metadata-only test wiring that doesn't care about plans.
func (h *Handler) WithBilling(b *billing.Service) *Handler {
	h.billing = b
	return h
}

// WithWebhooks wires an outbound-webhook publisher. When non-nil the
// handler emits webhook events (file.upload.confirmed,
// permission.granted, etc.) onto the JetStream subject so the
// delivery worker can fan them out to active subscribers. A nil
// publisher disables event emission (every Publish call becomes a
// no-op) so the metadata plane keeps working in tests / deployments
// without NATS configured. Mirrors the WithJobs nil-safe pattern.
func (h *Handler) WithWebhooks(p WebhookEventPublisher) *Handler {
	// Guard against passing a typed-nil concrete *webhooks.Publisher,
	// which would compare != nil under the interface comparison and
	// then NPE inside the emit helpers. The concrete publisher's
	// own methods are nil-safe (PublishFileEvent on a nil *Publisher
	// returns nil) — but going through the interface here keeps the
	// nil check at the boundary where it's obvious.
	if p == nil {
		h.webhooks = nil
		return h
	}
	if pub, ok := p.(*webhooks.Publisher); ok && pub == nil {
		h.webhooks = nil
		return h
	}
	h.webhooks = p
	return h
}

// WithDocuments wires the collaborative document service. When non-
// nil the /api/documents endpoints accept create / get / delta
// requests; when nil the endpoints respond 503 Service Unavailable
// so the metadata plane keeps working in deployments without the
// collab feature provisioned. Mirrors the WithSharing / WithBilling
// nil-safe patterns elsewhere in this file.
func (h *Handler) WithDocuments(s *document.Service) *Handler {
	h.documents = s
	return h
}

// WithAudit wires an audit service so security-relevant operations
// (permission grant/revoke, workspace update, admin ops) emit audit
// entries. A nil service silently drops audit logging while preserving
// the primary operation.
func (h *Handler) WithAudit(s *audit.Service) *Handler {
	h.audit = s
	return h
}

// WithSharing attaches a sharing service to the handler, enabling the
// /api/share-links and /api/guest-invites endpoints. Kept as a separate
// setter (rather than extending NewHandler) so existing test wiring
// stays backward compatible.
func (h *Handler) WithSharing(s *sharing.Service) *Handler {
	h.sharing = s
	return h
}

// WithSearch attaches a search service to the handler, enabling the
// /api/search endpoint.
func (h *Handler) WithSearch(s *search.Service) *Handler {
	h.search = s
	return h
}

// WithClientRooms attaches a client-room service so the /api/client-rooms
// endpoints stop responding 501 Not Implemented.
func (h *Handler) WithClientRooms(s *sharing.ClientRoomService) *Handler {
	h.clientRooms = s
	return h
}

// WithJobs attaches a NATS JetStream publisher so ConfirmUpload can
// enqueue preview / scan / index jobs. A nil publisher disables
// publishing (calls become no-ops), matching the logActivity pattern.
func (h *Handler) WithJobs(p *jobs.Publisher) *Handler {
	h.jobs = p
	return h
}

// WithNotifications wires the notification service. A nil service
// disables in-app notifications (notify* calls become no-ops) so the
// metadata plane keeps working in tests that don't care about
// notifications.
func (h *Handler) WithNotifications(s *notification.Service) *Handler {
	h.notifications = s
	return h
}

// WithEmail wires the transactional-email service so guest-invite
// creation can notify external recipients out-of-band. A nil
// service disables email delivery (the in-app notification path
// still fires for known users). Mirrors WithNotifications so test
// wiring stays cheap.
func (h *Handler) WithEmail(s *email.Service) *Handler {
	h.email = s
	return h
}

// WithPreviews wires the preview repository so the handler can serve
// preview download URLs without going through a service layer. A nil
// repository causes /api/files/{id}/preview-url to respond 404.
func (h *Handler) WithPreviews(r preview.Repository) *Handler {
	h.previews = r
	return h
}

// WithChangefeed wires the change-feed service. When non-nil every
// state-mutating logActivity call also records a durable Mutation
// and broadcasts it workspace-wide for the desktop sync SDK. A nil
// service silently disables the mirror so tests that don't care
// about sync keep working unchanged.
func (h *Handler) WithChangefeed(s *changefeed.Service) *Handler {
	h.changefeed = s
	return h
}

// WithStorageFactory wires a per-workspace storage client factory.
// When set, presigned-URL handlers prefer the factory's per-workspace
// client (resolved from workspace_storage_credentials) and fall back
// to the static client only when the factory cannot resolve one. A
// nil factory keeps the legacy behaviour of always using the static
// client.
func (h *Handler) WithStorageFactory(f *storage.ClientFactory) *Handler {
	h.storageFactory = f
	return h
}

// resolveStorage returns the storage client to use for workspaceID.
// It consults the per-workspace factory first; on miss or error it
// returns the static fallback. The factory itself encapsulates the
// fallback so the only case where this method returns nil is the
// "no storage configured at all" mode (S3_ENDPOINT empty + no
// fabric console wired).
func (h *Handler) resolveStorage(ctx context.Context, workspaceID uuid.UUID) *storage.Client {
	if h.storageFactory != nil {
		if c, err := h.storageFactory.ForWorkspace(ctx, workspaceID); err == nil {
			return c
		}
	}
	return h.storage
}

// notify is a nil-safe wrapper around the notification service,
// mirroring the logActivity pattern. Errors are logged and swallowed
// so notification failures never break the parent operation.
func (h *Handler) notify(ctx context.Context, fn func(*notification.Service) error) {
	if h.notifications == nil {
		return
	}
	if err := fn(h.notifications); err != nil {
		logging.FromContext(ctx).Error("drive notification dispatch failed", "err", err)
	}
}

// logActivity is a nil-safe wrapper so callers don't need to null-check
// every call-site. metadata may be nil.
//
// The activity log and the change feed are independently optional:
// each is gated on its own service being wired, so a deployment can
// enable one without the other. When the change feed is wired the
// activity entry is mirrored to change_log for state-mutation
// actions (file/folder create/rename/move/delete, file upload, tag
// add/remove, permission grant/revoke). Pure-read actions
// (file.download, file.bulk.download) are skipped — they do not
// affect any client's reconciled state. The mirror is synchronous
// so the durable cursor advances before the HTTP response, but a
// failure is logged and swallowed (the activity entry is still
// enqueued, the parent HTTP request still succeeds).
func (h *Handler) logActivity(ctx context.Context, action, resourceType string, resourceID uuid.UUID, metadata map[string]any) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(ctx)
	userID, _ := middleware.UserIDFromContext(ctx)
	if h.activity != nil {
		h.activity.LogAction(ctx, workspaceID, userID, action, resourceType, resourceID, metadata)
	}
	h.recordChange(ctx, workspaceID, userID, action, resourceType, resourceID, metadata)
}

// logActivityOnly mirrors logActivity but skips the change-feed leg.
// Bulk handlers (api/drive/bulk.go) use this in their per-item loop
// and call h.batchRecordChanges once at the end with the collected
// inputs, so a 100-item bulk operation produces a single multi-row
// change_log INSERT instead of 100 sequential ones. The activity
// log is unaffected — it is already async (LogAction enqueues onto
// a buffered channel) so per-item dispatch is cheap.
//
// CONTRACT for new bulk handlers: pair every logActivityOnly call
// inside the loop with a single batchRecordChanges call after the
// loop. Forgetting the second half silently drops the bulk
// operation from the sync feed even though the activity log
// records it. The opposite shape (one logActivity per item + no
// batchRecord) also works but throws away the amortisation; only
// use it for non-bulk paths.
func (h *Handler) logActivityOnly(ctx context.Context, action, resourceType string, resourceID uuid.UUID, metadata map[string]any) {
	if h.activity == nil {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(ctx)
	userID, _ := middleware.UserIDFromContext(ctx)
	h.activity.LogAction(ctx, workspaceID, userID, action, resourceType, resourceID, metadata)
}

// recordChange translates an activity action into a changefeed
// Mutation when the action is a state mutation. Skips read-only
// actions and short-circuits when the changefeed service is not
// wired. Errors are logged and swallowed — the change feed mirror
// is a best-effort overlay on top of the durable activity log, and
// a transient Postgres write failure here must not bubble up and
// fail the parent HTTP request.
func (h *Handler) recordChange(
	ctx context.Context,
	workspaceID, userID uuid.UUID,
	action, resourceType string,
	resourceID uuid.UUID,
	metadata map[string]any,
) {
	if h.changefeed == nil {
		return
	}
	if workspaceID == uuid.Nil || resourceID == uuid.Nil {
		return
	}
	kind, op, ok := changefeedKindOpFor(action, resourceType)
	if !ok {
		return
	}
	in := buildChangefeedInput(workspaceID, userID, kind, op, resourceID, metadata)
	if _, err := h.changefeed.Record(ctx, in); err != nil {
		logging.FromContext(ctx).Error("changefeed record failed; activity still enqueued",
			"action", action,
			"resource_type", resourceType,
			"resource_id", resourceID,
			"err", err,
		)
	}
}

// changeInput is the per-item payload a bulk handler accumulates
// inside its loop. The handler hands a slice of these to
// batchRecordChanges after the loop completes, and the changefeed
// service flushes them in a single multi-row INSERT.
type changeInput struct {
	Action       string
	ResourceType string
	ResourceID   uuid.UUID
	Metadata     map[string]any
}

// batchRecordChanges flushes a slice of per-item change inputs to
// the change feed in a single batch. Used by bulk handlers (move,
// copy, delete, download enumeration that mutates) to amortise the
// Postgres round-trip cost across an N-item operation. Read-only
// actions and actions that don't map to a change-feed kind/op are
// silently skipped here just like the per-item recordChange path.
func (h *Handler) batchRecordChanges(ctx context.Context, items []changeInput) {
	if h.changefeed == nil || len(items) == 0 {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(ctx)
	userID, _ := middleware.UserIDFromContext(ctx)
	if workspaceID == uuid.Nil {
		return
	}
	inputs := make([]changefeed.RecordInput, 0, len(items))
	for _, it := range items {
		if it.ResourceID == uuid.Nil {
			continue
		}
		kind, op, ok := changefeedKindOpFor(it.Action, it.ResourceType)
		if !ok {
			continue
		}
		inputs = append(inputs, buildChangefeedInput(workspaceID, userID, kind, op, it.ResourceID, it.Metadata))
	}
	if len(inputs) == 0 {
		return
	}
	if _, err := h.changefeed.BatchRecord(ctx, inputs); err != nil {
		logging.FromContext(ctx).Error("changefeed batch record failed",
			"count", len(inputs),
			"err", err,
		)
	}
}

// buildChangefeedInput is the shared builder used by both the
// per-item recordChange path and the bulk batchRecordChanges path.
// It lifts parent_id / name out of the activity metadata into
// dedicated columns and strips those keys from the metadata blob so
// the lifted value is not stored twice per row. Sync clients get
// the structured field; the original free-form metadata still
// flows through but minus the duplicated keys.
func buildChangefeedInput(
	workspaceID, userID uuid.UUID,
	kind, op string,
	resourceID uuid.UUID,
	metadata map[string]any,
) changefeed.RecordInput {
	in := changefeed.RecordInput{
		WorkspaceID: workspaceID,
		Kind:        kind,
		Op:          op,
		ResourceID:  resourceID,
	}
	if userID != uuid.Nil {
		actor := userID
		in.ActorID = &actor
	}
	// parentKeys is the (canonical) list of metadata keys whose
	// value is the resource's parent folder. "target" is the legacy
	// bulk-move convention used by BulkMove; the others come from
	// the single-file paths (file.go, folder.go). All are stripped
	// from the metadata blob (whether or not a value was lifted)
	// so the wire format is consistent across root vs. non-root
	// creates and single vs. bulk operations.
	//
	// TODO(activity-schema): replace the string-keyed metadata map
	// with a typed activityMetadata struct so the lift contract is
	// enforced at compile time. The current convention relies on
	// every caller spelling the right key (e.g. folder_id, not
	// folderID); a typo would silently leave parent_id = NULL on
	// the change_log row. A single-file refactor that introduces
	// the struct and migrates all callers is tracked in the
	// desktop-sync backlog and is intentionally out of scope here
	// to keep the changefeed P1a PR focused.
	parentKeys := [...]string{"folder_id", "parent_folder_id", "new_parent_folder_id", "target"}
	var parentKeyPresent bool
	for _, k := range parentKeys {
		raw, present := metadata[k]
		if !present {
			continue
		}
		parentKeyPresent = true
		if in.ParentID != nil {
			continue
		}
		if pid, ok := uuidFromAny(raw); ok {
			in.ParentID = &pid
		}
	}
	var nameKeyPresent bool
	if raw, present := metadata["name"]; present {
		nameKeyPresent = true
		if n, ok := raw.(string); ok {
			in.Name = n
		}
	}
	// Strip the lifted keys from the metadata blob so the value is
	// not stored redundantly in change_log.metadata alongside the
	// dedicated columns. Crucially, we also strip when the key is
	// present but the value couldn't be lifted (typed-nil pointer,
	// empty string, etc.) — the structured column will be NULL in
	// that case and a stray "parent_folder_id": null inside the
	// JSONB blob would just be a wire-format inconsistency between
	// root and non-root creates. We shallow-copy the caller's map
	// so the activity log entry (which receives the original) is
	// untouched.
	if len(metadata) > 0 && (parentKeyPresent || nameKeyPresent) {
		trimmed := make(map[string]any, len(metadata))
		for k, v := range metadata {
			trimmed[k] = v
		}
		if parentKeyPresent {
			for _, k := range parentKeys {
				delete(trimmed, k)
			}
		}
		if nameKeyPresent {
			delete(trimmed, "name")
		}
		if len(trimmed) > 0 {
			in.Metadata = trimmed
		}
	} else if len(metadata) > 0 {
		in.Metadata = metadata
	}
	return in
}

// uuidFromAny extracts a uuid.UUID from common dynamic-typed values
// found in the activity metadata map: a uuid.UUID itself, its
// string form, or a json.Number when the map came back through a
// JSON unmarshal somewhere upstream. Returns (uuid.Nil, false) for
// anything else so callers know to skip the field.
func uuidFromAny(v any) (uuid.UUID, bool) {
	switch x := v.(type) {
	case uuid.UUID:
		return x, true
	case *uuid.UUID:
		if x == nil {
			return uuid.Nil, false
		}
		return *x, true
	case string:
		id, err := uuid.Parse(x)
		if err != nil {
			return uuid.Nil, false
		}
		return id, true
	}
	return uuid.Nil, false
}

// changefeedReadOnlyActions enumerates the activity actions that are
// explicitly excluded from the change feed because they don't mutate
// any client-reconciled state (download, bulk-download). Adding a new
// read-only action here is the explicit signal that "this action was
// considered for the change feed and deliberately skipped" — separate
// from an action being absent because someone forgot to map it.
var changefeedReadOnlyActions = map[string]struct{}{
	activity.ActionFileDownload:     {},
	activity.ActionFileBulkDownload: {},
}

// changefeedKindOpFor maps an activity action string to a
// (kind, op) pair appropriate for the change feed, or ("", "", false)
// when the action is in the changefeedReadOnlyActions set.
//
// The mapping is deliberately exhaustive over activity.AllActions — a
// new Action* constant that is neither mapped here nor added to the
// read-only set will be caught by
// TestChangefeedKindOpFor_ExhaustiveOverAllActions.
func changefeedKindOpFor(action, resourceType string) (string, string, bool) {
	// TODO(desktop-sdk): use resourceType to split permission events
	// into KindFilePermission / KindFolderPermission once sync
	// clients need to distinguish them. Until then, the kind is
	// derivable from the action string alone — every action constant
	// encodes its resource family in the prefix (file.* vs folder.*).
	_ = resourceType
	switch action {
	case activity.ActionFileCreate:
		return changefeed.KindFile, changefeed.OpCreate, true
	case activity.ActionFileUpload:
		// A successful upload is a content change on an existing
		// file row (the row was created by file.create earlier).
		return changefeed.KindFile, changefeed.OpUpdate, true
	case activity.ActionFileRename:
		return changefeed.KindFile, changefeed.OpRename, true
	case activity.ActionFileMove, activity.ActionFileBulkMove:
		return changefeed.KindFile, changefeed.OpMove, true
	case activity.ActionFileDelete, activity.ActionFileBulkDelete:
		return changefeed.KindFile, changefeed.OpDelete, true
	case activity.ActionFileTagAdd, activity.ActionFileTagRemove:
		return changefeed.KindFile, changefeed.OpUpdate, true
	case activity.ActionFileBulkCopy:
		// Copy creates a new file row, mirroring file.create.
		return changefeed.KindFile, changefeed.OpCreate, true
	case activity.ActionFolderCreate:
		return changefeed.KindFolder, changefeed.OpCreate, true
	case activity.ActionFolderRename:
		return changefeed.KindFolder, changefeed.OpRename, true
	case activity.ActionFolderMove:
		return changefeed.KindFolder, changefeed.OpMove, true
	case activity.ActionFolderDelete:
		return changefeed.KindFolder, changefeed.OpDelete, true
	case activity.ActionDocumentCreate:
		return changefeed.KindDocument, changefeed.OpCreate, true
	case activity.ActionDocumentRename:
		return changefeed.KindDocument, changefeed.OpRename, true
	case activity.ActionDocumentDelete:
		return changefeed.KindDocument, changefeed.OpDelete, true
	case activity.ActionDocumentChangeCollabMode:
		return changefeed.KindDocument, changefeed.OpUpdate, true
	case activity.ActionPermGrant:
		return changefeed.KindPermission, changefeed.OpCreate, true
	case activity.ActionPermRevoke:
		return changefeed.KindPermission, changefeed.OpDelete, true
	}
	return "", "", false
}

// logAudit is a nil-safe wrapper mirroring logActivity.
func (h *Handler) logAudit(ctx context.Context, r *http.Request, action, resourceType string, resourceID *uuid.UUID, metadata map[string]any) {
	if h.audit == nil {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(ctx)
	userID, _ := middleware.UserIDFromContext(ctx)
	var actor *uuid.UUID
	if userID != uuid.Nil {
		actor = &userID
	}
	h.audit.LogAction(ctx, workspaceID, actor, action, resourceType, resourceID, r, metadata)
}
