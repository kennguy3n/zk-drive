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
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/billing"
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
	users          *user.Service
	storage        *storage.Client
	storageFactory *storage.ClientFactory
	permissions    *permission.Service
	activity       *activity.Service
	sharing        *sharing.Service
	search         *search.Service
	clientRooms    *sharing.ClientRoomService
	jobs           *jobs.Publisher
	notifications  *notification.Service
	previews       preview.Repository
	audit          *audit.Service
	billing        *billing.Service
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

// WithPreviews wires the preview repository so the handler can serve
// preview download URLs without going through a service layer. A nil
// repository causes /api/files/{id}/preview-url to respond 404.
func (h *Handler) WithPreviews(r preview.Repository) *Handler {
	h.previews = r
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
		slog.ErrorContext(ctx, "drive notification dispatch failed", "err", err)
	}
}

// logActivity is a nil-safe wrapper so callers don't need to null-check
// every call-site. metadata may be nil.
func (h *Handler) logActivity(ctx context.Context, action, resourceType string, resourceID uuid.UUID, metadata map[string]any) {
	if h.activity == nil {
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(ctx)
	userID, _ := middleware.UserIDFromContext(ctx)
	h.activity.LogAction(ctx, workspaceID, userID, action, resourceType, resourceID, metadata)
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
