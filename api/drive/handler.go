package drive

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
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

// activityMaxPageSize must stay in sync with activity.normalizePaging so
// the handler can echo the effective limit back to the client without
// lying about how many rows the repository actually returned.
const activityMaxPageSize = 200

// Handler serves workspace / folder / file HTTP endpoints.
//
// storage is optional: when nil, the upload-url / confirm-upload /
// download-url endpoints respond with 501 Not Implemented so the server
// can still serve metadata-only APIs without a zk-object-fabric gateway
// configured.
type Handler struct {
	pool        *pgxpool.Pool
	workspaces  *workspace.Service
	folders     *folder.Service
	files       *file.Service
	users       *user.Service
	storage     *storage.Client
	permissions *permission.Service
	activity    *activity.Service
	sharing     *sharing.Service
	search      *search.Service
	clientRooms   *sharing.ClientRoomService
	jobs          *jobs.Publisher
	notifications *notification.Service
	previews      preview.Repository
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

// notify is a nil-safe wrapper around the notification service,
// mirroring the logActivity pattern. Errors are logged and swallowed
// so notification failures never break the parent operation.
func (h *Handler) notify(ctx context.Context, fn func(*notification.Service) error) {
	if h.notifications == nil {
		return
	}
	if err := fn(h.notifications); err != nil {
		log.Printf("notification error: %v", err)
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

// Workspace DTOs -------------------------------------------------------------

type createWorkspaceRequest struct {
	Name string `json:"name"`
}

type updateWorkspaceRequest struct {
	Name              *string `json:"name,omitempty"`
	StorageQuotaBytes *int64  `json:"storage_quota_bytes,omitempty"`
	Tier              *string `json:"tier,omitempty"`
}

// Folder DTOs ---------------------------------------------------------------

type createFolderRequest struct {
	WorkspaceID    string  `json:"workspace_id"`
	ParentFolderID *string `json:"parent_folder_id,omitempty"`
	Name           string  `json:"name"`
}

type renameFolderRequest struct {
	Name string `json:"name"`
}

type moveFolderRequest struct {
	NewParentFolderID *string `json:"new_parent_folder_id,omitempty"`
}

// File DTOs -----------------------------------------------------------------

type createFileRequest struct {
	FolderID string `json:"folder_id"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type,omitempty"`
}

type updateFileRequest struct {
	Name string `json:"name"`
}

type moveFileRequest struct {
	FolderID string `json:"folder_id"`
}

// Upload / download DTOs ----------------------------------------------------

type uploadURLRequest struct {
	FolderID string `json:"folder_id"`
	Filename string `json:"filename"`
	MimeType string `json:"mime_type,omitempty"`
}

type uploadURLResponse struct {
	UploadURL string    `json:"upload_url"`
	UploadID  uuid.UUID `json:"upload_id"`
	ObjectKey string    `json:"object_key"`
}

type confirmUploadRequest struct {
	FileID    string `json:"file_id"`
	ObjectKey string `json:"object_key"`
	SizeBytes int64  `json:"size_bytes"`
	Checksum  string `json:"checksum,omitempty"`
}

type downloadURLResponse struct {
	DownloadURL string `json:"download_url"`
	ObjectKey   string `json:"object_key"`
}

// Workspace handlers --------------------------------------------------------

// ListWorkspaces returns all workspaces the authenticated user belongs to.
func (h *Handler) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	list, err := h.workspaces.ListForUser(r.Context(), userID)
	if err != nil {
		http.Error(w, "list workspaces: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": list})
}

// CreateWorkspace creates a new workspace and makes the authenticated user
// its admin.
func (h *Handler) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req createWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Look up the current user to copy identity + password hash into the new
	// workspace so the creator remains a member.
	currentWSID, _ := middleware.WorkspaceIDFromContext(r.Context())
	current, err := h.users.GetByID(r.Context(), currentWSID, userID)
	if err != nil {
		http.Error(w, "load current user: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ws, err := h.createWorkspaceTx(r.Context(), req.Name, current)
	if err != nil {
		http.Error(w, "create workspace: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

// createWorkspaceTx creates the workspace, adds the current user as its
// admin member, and sets workspace.owner_user_id — all inside a single
// transaction so partial failures don't leave orphaned rows.
func (h *Handler) createWorkspaceTx(ctx context.Context, name string, current *user.User) (*workspace.Workspace, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ws, err := h.workspaces.CreateTx(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	newUser := &user.User{
		WorkspaceID:  ws.ID,
		Email:        current.Email,
		Name:         current.Name,
		PasswordHash: current.PasswordHash,
		Role:         user.RoleAdmin,
	}
	if err := h.users.CreatePreservingHashTx(ctx, tx, newUser); err != nil {
		return nil, err
	}
	if err := h.workspaces.SetOwnerTx(ctx, tx, ws.ID, newUser.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	ws.OwnerUserID = &newUser.ID
	return ws, nil
}

// GetWorkspace returns workspace details. The authenticated session must be
// bound to this workspace.
func (h *Handler) GetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := h.requireWorkspaceMatch(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// UpdateWorkspace updates the workspace name, tier, or quota. Admin only.
func (h *Handler) UpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	ws, err := h.requireWorkspaceMatch(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var req updateWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.Name != nil {
		ws.Name = *req.Name
	}
	if req.Tier != nil {
		ws.Tier = *req.Tier
	}
	if req.StorageQuotaBytes != nil {
		ws.StorageQuotaBytes = *req.StorageQuotaBytes
	}
	if err := h.workspaces.Update(r.Context(), ws); err != nil {
		http.Error(w, "update workspace: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// Folder handlers -----------------------------------------------------------

// CreateFolder creates a new folder scoped to the authenticated workspace.
func (h *Handler) CreateFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req createFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.WorkspaceID != "" {
		reqWS, err := uuid.Parse(req.WorkspaceID)
		if err != nil || reqWS != workspaceID {
			http.Error(w, "workspace_id mismatch", http.StatusForbidden)
			return
		}
	}
	var parentID *uuid.UUID
	if req.ParentFolderID != nil && *req.ParentFolderID != "" {
		pid, err := uuid.Parse(*req.ParentFolderID)
		if err != nil {
			http.Error(w, "invalid parent_folder_id", http.StatusBadRequest)
			return
		}
		parentID = &pid
	}
	f, err := h.folders.Create(r.Context(), workspaceID, parentID, req.Name, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFolderCreate, permission.ResourceFolder, f.ID, map[string]any{
		"name":             f.Name,
		"parent_folder_id": f.ParentFolderID,
	})
	writeJSON(w, http.StatusCreated, f)
}

// GetFolder returns folder details plus child folders and files.
func (h *Handler) GetFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	f, err := h.folders.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, f.ID, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	children, err := h.folders.ListChildren(r.Context(), workspaceID, &id)
	if err != nil {
		http.Error(w, "list children: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fileList, err := h.files.ListByFolder(r.Context(), workspaceID, id)
	if err != nil {
		http.Error(w, "list files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"folder":   f,
		"children": children,
		"files":    fileList,
	})
}

// ListFolders returns root-level folders (when parent_folder_id=root) or
// direct children of a given parent.
func (h *Handler) ListFolders(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	if wsParam := r.URL.Query().Get("workspace_id"); wsParam != "" {
		wsID, err := uuid.Parse(wsParam)
		if err != nil || wsID != workspaceID {
			http.Error(w, "workspace_id mismatch", http.StatusForbidden)
			return
		}
	}
	parentParam := r.URL.Query().Get("parent_folder_id")
	var parentID *uuid.UUID
	if parentParam != "" && parentParam != "root" {
		pid, err := uuid.Parse(parentParam)
		if err != nil {
			http.Error(w, "invalid parent_folder_id", http.StatusBadRequest)
			return
		}
		parentID = &pid
	}
	list, err := h.folders.ListChildren(r.Context(), workspaceID, parentID)
	if err != nil {
		http.Error(w, "list folders: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": list})
}

// RenameFolder updates a folder's name in-place.
func (h *Handler) RenameFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req renameFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, id, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	f, err := h.folders.Rename(r.Context(), workspaceID, id, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFolderRename, permission.ResourceFolder, f.ID, map[string]any{
		"name": f.Name,
	})
	writeJSON(w, http.StatusOK, f)
}

// DeleteFolder soft-deletes a folder subtree.
func (h *Handler) DeleteFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, id, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.folders.Delete(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFolderDelete, permission.ResourceFolder, id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// MoveFolder relocates a folder under a new parent.
func (h *Handler) MoveFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req moveFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	var parentID *uuid.UUID
	if req.NewParentFolderID != nil && *req.NewParentFolderID != "" {
		pid, perr := uuid.Parse(*req.NewParentFolderID)
		if perr != nil {
			http.Error(w, "invalid new_parent_folder_id", http.StatusBadRequest)
			return
		}
		parentID = &pid
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, id, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	if parentID != nil {
		if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, *parentID, permission.RoleEditor); err != nil {
			writeServiceError(w, err)
			return
		}
	}
	f, err := h.folders.Move(r.Context(), workspaceID, id, parentID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFolderMove, permission.ResourceFolder, f.ID, map[string]any{
		"new_parent_folder_id": f.ParentFolderID,
	})
	writeJSON(w, http.StatusOK, f)
}

// File handlers -------------------------------------------------------------

// CreateFile inserts a file metadata row (no version yet).
func (h *Handler) CreateFile(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	var req createFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	folderID, err := uuid.Parse(req.FolderID)
	if err != nil {
		http.Error(w, "invalid folder_id", http.StatusBadRequest)
		return
	}
	if _, err := h.folders.GetByID(r.Context(), workspaceID, folderID); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, folderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	f, err := h.files.Create(r.Context(), workspaceID, folderID, req.Name, req.MimeType, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileCreate, permission.ResourceFile, f.ID, map[string]any{
		"name":      f.Name,
		"folder_id": f.FolderID,
	})
	writeJSON(w, http.StatusCreated, f)
}

// GetFile returns file metadata.
func (h *Handler) GetFile(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	f, err := h.files.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, f.ID, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// UpdateFile renames a file.
func (h *Handler) UpdateFile(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req updateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	f, err := h.files.Rename(r.Context(), workspaceID, id, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileRename, permission.ResourceFile, f.ID, map[string]any{
		"name": f.Name,
	})
	writeJSON(w, http.StatusOK, f)
}

// DeleteFile soft-deletes a file.
func (h *Handler) DeleteFile(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.files.Delete(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileDelete, permission.ResourceFile, id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// MoveFile relocates a file to a different folder.
func (h *Handler) MoveFile(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req moveFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	folderID, err := uuid.Parse(req.FolderID)
	if err != nil {
		http.Error(w, "invalid folder_id", http.StatusBadRequest)
		return
	}
	if _, err := h.folders.GetByID(r.Context(), workspaceID, folderID); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, folderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	f, err := h.files.Move(r.Context(), workspaceID, id, folderID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileMove, permission.ResourceFile, f.ID, map[string]any{
		"folder_id": f.FolderID,
	})
	writeJSON(w, http.StatusOK, f)
}

// ListFileVersions returns every version of a file.
func (h *Handler) ListFileVersions(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	versions, err := h.files.ListVersions(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// Upload / download handlers -----------------------------------------------

// UploadURL generates a presigned PUT URL that lets the caller upload a
// single file version directly to zk-object-fabric. It creates the file
// metadata row up front so the client can reference a stable file ID when
// it later calls ConfirmUpload. The returned object_key is opaque to the
// client; it must be echoed back verbatim on confirm so the server records
// the exact key it signed.
func (h *Handler) UploadURL(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req uploadURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	folderID, err := uuid.Parse(req.FolderID)
	if err != nil {
		http.Error(w, "invalid folder_id", http.StatusBadRequest)
		return
	}
	if _, err := h.folders.GetByID(r.Context(), workspaceID, folderID); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, folderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}

	f, err := h.files.Create(r.Context(), workspaceID, folderID, req.Filename, req.MimeType, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	versionID := uuid.New()
	objectKey := storage.NewObjectKey(workspaceID, f.ID, versionID)

	url, err := h.storage.GenerateUploadURL(r.Context(), objectKey, req.MimeType, storage.DefaultPresignExpiry)
	if err != nil {
		http.Error(w, "generate upload url: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, uploadURLResponse{
		UploadURL: url,
		UploadID:  f.ID,
		ObjectKey: objectKey,
	})
}

// ConfirmUpload records a newly uploaded object as a FileVersion and
// advances the file's current_version pointer. Callers must invoke this
// after the direct-to-storage PUT succeeds; otherwise the file row exists
// without a current version and Downloads will 404.
func (h *Handler) ConfirmUpload(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req confirmUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.ObjectKey == "" {
		http.Error(w, "object_key is required", http.StatusBadRequest)
		return
	}
	if req.SizeBytes < 0 {
		http.Error(w, "size_bytes must be non-negative", http.StatusBadRequest)
		return
	}
	fileID, err := uuid.Parse(req.FileID)
	if err != nil {
		http.Error(w, "invalid file_id", http.StatusBadRequest)
		return
	}
	f, err := h.files.GetByID(r.Context(), workspaceID, fileID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	// The object_key is client-supplied; bind it to the caller's workspace
	// and file so a malicious client cannot confirm against a key it did
	// not legitimately receive from UploadURL (cross-tenant read via
	// DownloadURL).
	expectedPrefix := workspaceID.String() + "/" + f.ID.String() + "/"
	if !strings.HasPrefix(req.ObjectKey, expectedPrefix) {
		http.Error(w, "object_key does not belong to this file", http.StatusForbidden)
		return
	}

	v := &file.FileVersion{
		FileID:    f.ID,
		ObjectKey: req.ObjectKey,
		SizeBytes: req.SizeBytes,
		Checksum:  req.Checksum,
		CreatedBy: userID,
	}
	if err := h.files.ConfirmVersion(r.Context(), workspaceID, v); err != nil {
		writeServiceError(w, err)
		return
	}

	updated, err := h.files.GetByID(r.Context(), workspaceID, f.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileUpload, permission.ResourceFile, f.ID, map[string]any{
		"version_id": v.ID,
		"size_bytes": v.SizeBytes,
	})
	// Fan out post-upload work (preview, scan, index) via JetStream.
	// All three publishers are nil-safe so the handler behaves
	// identically when NATS is not configured locally. Publish errors
	// are logged and ignored so a flaky broker never fails an
	// otherwise-successful upload — workers can be re-triggered later.
	h.publishPostUploadJobs(r.Context(), f.ID, v.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"file":    updated,
		"version": v,
	})
}

// publishPostUploadJobs dispatches preview / scan / index jobs after a
// successful ConfirmUpload. Factored out so tests can stub it and so
// future subjects can be added in one place.
func (h *Handler) publishPostUploadJobs(ctx context.Context, fileID, versionID uuid.UUID) {
	if h.jobs == nil {
		return
	}
	if err := h.jobs.PublishPreview(ctx, fileID, versionID); err != nil {
		log.Printf("drive: publish preview job: %v", err)
	}
	if err := h.jobs.PublishScan(ctx, fileID, versionID); err != nil {
		log.Printf("drive: publish scan job: %v", err)
	}
	if err := h.jobs.PublishIndex(ctx, fileID, versionID); err != nil {
		log.Printf("drive: publish index job: %v", err)
	}
}

// DownloadURL returns a presigned GET URL for the file's current version.
func (h *Handler) DownloadURL(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	f, err := h.files.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, f.ID, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	if f.CurrentVersionID == nil {
		http.Error(w, "file has no current version", http.StatusNotFound)
		return
	}
	versions, err := h.files.ListVersions(r.Context(), workspaceID, f.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var current *file.FileVersion
	for _, v := range versions {
		if v.ID == *f.CurrentVersionID {
			current = v
			break
		}
	}
	if current == nil {
		http.Error(w, "current version not found", http.StatusNotFound)
		return
	}
	url, err := h.storage.GenerateDownloadURL(r.Context(), current.ObjectKey, storage.DefaultPresignExpiry)
	if err != nil {
		http.Error(w, "generate download url: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileDownload, permission.ResourceFile, f.ID, map[string]any{
		"version_id": current.ID,
	})
	writeJSON(w, http.StatusOK, downloadURLResponse{
		DownloadURL: url,
		ObjectKey:   current.ObjectKey,
	})
}

// Permission handlers ------------------------------------------------------

type grantPermissionRequest struct {
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	GranteeType  string  `json:"grantee_type"`
	GranteeID    string  `json:"grantee_id"`
	Role         string  `json:"role"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
}

// ListPermissions returns every grant on a resource. Callers must supply
// resource_type and resource_id query params. Scoped to the authenticated
// workspace so one tenant never sees another's grants.
func (h *Handler) ListPermissions(w http.ResponseWriter, r *http.Request) {
	if h.permissions == nil {
		http.Error(w, "permissions not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())

	resourceType := r.URL.Query().Get("resource_type")
	resourceIDParam := r.URL.Query().Get("resource_id")
	if resourceType == "" || resourceIDParam == "" {
		http.Error(w, "resource_type and resource_id are required", http.StatusBadRequest)
		return
	}
	resourceID, err := uuid.Parse(resourceIDParam)
	if err != nil {
		http.Error(w, "invalid resource_id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, resourceType, resourceID); err != nil {
		writeServiceError(w, err)
		return
	}
	list, err := h.permissions.ListForResource(r.Context(), workspaceID, resourceType, resourceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissions": list})
}

// GrantPermission creates a new permission. Admin-only in Phase 1.
func (h *Handler) GrantPermission(w http.ResponseWriter, r *http.Request) {
	if h.permissions == nil {
		http.Error(w, "permissions not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())

	var req grantPermissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	resourceID, err := uuid.Parse(req.ResourceID)
	if err != nil {
		http.Error(w, "invalid resource_id", http.StatusBadRequest)
		return
	}
	granteeID, err := uuid.Parse(req.GranteeID)
	if err != nil {
		http.Error(w, "invalid grantee_id", http.StatusBadRequest)
		return
	}

	// Resources must live in the same workspace — otherwise an admin in
	// workspace A could grant access to a resource in workspace B by
	// guessing its UUID. This is the core tenant-isolation check for the
	// permissions API.
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, req.ResourceType, resourceID); err != nil {
		writeServiceError(w, err)
		return
	}
	if req.GranteeType == permission.GranteeUser {
		if err := h.assertUserInWorkspace(r.Context(), workspaceID, granteeID); err != nil {
			writeServiceError(w, err)
			return
		}
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			http.Error(w, "invalid expires_at (expected RFC3339)", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}

	p, err := h.permissions.Grant(r.Context(), workspaceID, req.ResourceType, resourceID, req.GranteeType, granteeID, req.Role, expiresAt)
	if err != nil {
		writePermissionError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionPermGrant, req.ResourceType, resourceID, map[string]any{
		"permission_id": p.ID,
		"grantee_type":  p.GranteeType,
		"grantee_id":    p.GranteeID,
		"role":          p.Role,
	})
	writeJSON(w, http.StatusCreated, p)
}

// RevokePermission deletes a grant. Admin-only.
func (h *Handler) RevokePermission(w http.ResponseWriter, r *http.Request) {
	if h.permissions == nil {
		http.Error(w, "permissions not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	// Fetch first so we can log the resource context on revoke.
	p, err := h.permissions.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.permissions.Revoke(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionPermRevoke, p.ResourceType, p.ResourceID, map[string]any{
		"permission_id": p.ID,
		"grantee_type":  p.GranteeType,
		"grantee_id":    p.GranteeID,
		"role":          p.Role,
	})
	w.WriteHeader(http.StatusNoContent)
}

// assertResourceAccess gates a handler operation behind the
// permission layer using the inheritance-aware check (ARCHITECTURE.md
// §7.2). Workspace admins always pass; other authenticated users must
// hold a direct or inherited grant of at least minRole on the
// resource. When the permissions service is nil (metadata-only test
// wiring) the check is skipped so callers that opt out of permission
// enforcement keep working. Returns nil on allow, a forbiddenErr on
// deny, or the underlying repository error on lookup failure.
func (h *Handler) assertResourceAccess(ctx context.Context, resourceType string, resourceID uuid.UUID, minRole string) error {
	if h.permissions == nil {
		return nil
	}
	if role, _ := middleware.RoleFromContext(ctx); role == user.RoleAdmin {
		return nil
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(ctx)
	if !ok {
		return forbiddenErr{"missing workspace context"}
	}
	userID, ok := middleware.UserIDFromContext(ctx)
	if !ok {
		return forbiddenErr{"missing user context"}
	}
	allowed, err := h.permissions.HasAccessWithInheritance(ctx, workspaceID, resourceType, resourceID, permission.GranteeUser, userID, minRole)
	if err != nil {
		return err
	}
	if !allowed {
		return forbiddenErr{"insufficient permissions on resource"}
	}
	return nil
}

// assertResourceInWorkspace verifies that the given resource belongs to the
// workspace. Used by every permission endpoint to prevent cross-tenant
// grants.
func (h *Handler) assertResourceInWorkspace(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) error {
	switch resourceType {
	case permission.ResourceFolder:
		_, err := h.folders.GetByID(ctx, workspaceID, resourceID)
		return err
	case permission.ResourceFile:
		_, err := h.files.GetByID(ctx, workspaceID, resourceID)
		return err
	default:
		return badRequestErr{"invalid resource_type"}
	}
}

// assertUserInWorkspace verifies that granteeID corresponds to a user in
// this workspace. (Guest grantees are opaque UUIDs; they don't require a
// users-table lookup.)
func (h *Handler) assertUserInWorkspace(ctx context.Context, workspaceID, userID uuid.UUID) error {
	_, err := h.users.GetByID(ctx, workspaceID, userID)
	return err
}

func writePermissionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, permission.ErrInvalidRole),
		errors.Is(err, permission.ErrInvalidResourceType),
		errors.Is(err, permission.ErrInvalidGranteeType):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeServiceError(w, err)
	}
}

// Activity handlers --------------------------------------------------------

// ListActivity returns paginated activity_log entries for the authenticated
// workspace. If workspace_id query param is provided it must match the
// tenant bound to the session.
func (h *Handler) ListActivity(w http.ResponseWriter, r *http.Request) {
	if h.activity == nil {
		http.Error(w, "activity not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	if wsParam := r.URL.Query().Get("workspace_id"); wsParam != "" {
		wsID, err := uuid.Parse(wsParam)
		if err != nil || wsID != workspaceID {
			http.Error(w, "workspace_id mismatch", http.StatusForbidden)
			return
		}
	}
	limit := parseIntParam(r.URL.Query().Get("limit"), 50)
	offset := parseIntParam(r.URL.Query().Get("offset"), 0)
	// Keep in sync with activity.normalizePaging — the handler echoes the
	// effective limit back to the client, so a client doing
	// len(entries) < limit to detect end-of-stream stays correct when the
	// repo silently caps oversized requests.
	if limit <= 0 {
		limit = 50
	}
	if limit > activityMaxPageSize {
		limit = activityMaxPageSize
	}

	var (
		list []*activity.LogEntry
		err  error
	)
	if rt, rid := r.URL.Query().Get("resource_type"), r.URL.Query().Get("resource_id"); rt != "" && rid != "" {
		resourceID, perr := uuid.Parse(rid)
		if perr != nil {
			http.Error(w, "invalid resource_id", http.StatusBadRequest)
			return
		}
		list, err = h.activity.ListByResource(r.Context(), workspaceID, rt, resourceID, limit, offset)
	} else {
		list, err = h.activity.List(r.Context(), workspaceID, limit, offset)
	}
	if err != nil {
		http.Error(w, "list activity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": list,
		"limit":   limit,
		"offset":  offset,
	})
}

// parseIntParam parses a query-string int with a default. Negative values
// fall back to def so a malicious "?limit=-1" can't break the SQL.
func parseIntParam(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// Shared helpers ------------------------------------------------------------

func (h *Handler) requireWorkspaceMatch(r *http.Request) (*workspace.Workspace, error) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	paramID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return nil, badRequestErr{"invalid id"}
	}
	if paramID != workspaceID {
		return nil, forbiddenErr{"workspace mismatch"}
	}
	ws, err := h.workspaces.GetByID(r.Context(), workspaceID)
	if err != nil {
		return nil, err
	}
	return ws, nil
}

type badRequestErr struct{ msg string }

func (e badRequestErr) Error() string { return e.msg }

type forbiddenErr struct{ msg string }

func (e forbiddenErr) Error() string { return e.msg }

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, folder.ErrNotFound),
		errors.Is(err, file.ErrNotFound),
		errors.Is(err, workspace.ErrNotFound),
		errors.Is(err, user.ErrNotFound),
		errors.Is(err, permission.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, folder.ErrInvalidName), errors.Is(err, folder.ErrInvalidParent), errors.Is(err, file.ErrInvalidName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		var br badRequestErr
		var fb forbiddenErr
		if errors.As(err, &br) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if errors.As(err, &fb) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// Sharing DTOs -------------------------------------------------------------

type createShareLinkRequest struct {
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	Password     string  `json:"password,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	MaxDownloads *int    `json:"max_downloads,omitempty"`
}

type resolveShareLinkRequest struct {
	Password string `json:"password,omitempty"`
}

type createGuestInviteRequest struct {
	Email     string  `json:"email"`
	FolderID  string  `json:"folder_id"`
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// CreateShareLink creates a share link on a folder or file owned by the
// authenticated workspace. Admin-only per ARCHITECTURE.md §7.3 —
// sharing configuration is a sensitive operation.
func (h *Handler) CreateShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createShareLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if !sharing.IsValidResourceType(req.ResourceType) {
		http.Error(w, "invalid resource_type", http.StatusBadRequest)
		return
	}
	resourceID, err := uuid.Parse(req.ResourceID)
	if err != nil {
		http.Error(w, "invalid resource_id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, req.ResourceType, resourceID); err != nil {
		writeServiceError(w, err)
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			http.Error(w, "invalid expires_at (expected RFC3339)", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}
	link, err := h.sharing.CreateShareLink(r.Context(), workspaceID, req.ResourceType, resourceID, req.Password, expiresAt, req.MaxDownloads, userID)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	h.notify(r.Context(), func(n *notification.Service) error {
		return n.NotifyShareLinkCreated(r.Context(), workspaceID, userID, link.ID, req.ResourceType, resourceID)
	})
	writeJSON(w, http.StatusCreated, link)
}

// ResolveShareLink is the public (no-auth) endpoint used by anyone who
// holds a share-link token. When the link is password-protected the
// caller should supply the password in the JSON body of a POST; GET
// without a body is accepted for unprotected links so shares work from
// a plain browser address bar.
func (h *Handler) ResolveShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	token := chi.URLParam(r, "token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	var password string
	if r.Method == http.MethodPost && r.ContentLength > 0 {
		var req resolveShareLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		password = req.Password
	}
	link, err := h.sharing.ResolveShareLink(r.Context(), token, password)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, link)
}

// RevokeShareLink deletes a share link. Admin-only.
func (h *Handler) RevokeShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.sharing.RevokeShareLink(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateGuestInvite creates a guest invite and the matching permission
// grant on the target folder. Admin-only.
func (h *Handler) CreateGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createGuestInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	folderID, err := uuid.Parse(req.FolderID)
	if err != nil {
		http.Error(w, "invalid folder_id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, permission.ResourceFolder, folderID); err != nil {
		writeServiceError(w, err)
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			http.Error(w, "invalid expires_at (expected RFC3339)", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}
	inv, err := h.sharing.CreateGuestInvite(r.Context(), workspaceID, req.Email, folderID, req.Role, expiresAt, userID)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	// Best-effort invitee notification: only users who already have an
	// account in this workspace get an in-app notification. External
	// invitees receive an email out-of-band (out of scope for this
	// sprint).
	if h.users != nil && h.notifications != nil {
		if u, uerr := h.users.GetByEmail(r.Context(), workspaceID, req.Email); uerr == nil && u != nil {
			h.notify(r.Context(), func(n *notification.Service) error {
				return n.NotifyGuestInviteSent(r.Context(), workspaceID, u.ID, inv.ID, folderID, req.Email)
			})
		}
	}
	writeJSON(w, http.StatusCreated, inv)
}

// AcceptGuestInvite marks an invite accepted. Authenticated endpoint —
// the caller must already hold a token bound to the invite's workspace
// (in practice the token is issued by the invite-acceptance flow).
func (h *Handler) AcceptGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	inv, err := h.sharing.AcceptGuestInvite(r.Context(), workspaceID, id)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	h.notify(r.Context(), func(n *notification.Service) error {
		return n.NotifyGuestInviteAccepted(r.Context(), workspaceID, inv.CreatedBy, inv.ID, inv.Email)
	})
	writeJSON(w, http.StatusOK, inv)
}

// RevokeGuestInvite deletes an invite. Admin-only.
func (h *Handler) RevokeGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		http.Error(w, "sharing not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.sharing.RevokeGuestInvite(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Client room handlers ----------------------------------------------------

type createClientRoomRequest struct {
	Name           string  `json:"name"`
	Password       string  `json:"password,omitempty"`
	ExpiresAt      *string `json:"expires_at,omitempty"`
	DropboxEnabled bool    `json:"dropbox_enabled,omitempty"`
}

// clientRoomResponse envelopes the room row alongside the share-link
// token so the client has everything it needs to display the public
// URL after a single round-trip. We don't inline the entire ShareLink
// struct — only the token and link id — to keep the client API narrow.
type clientRoomResponse struct {
	*sharing.ClientRoom
	ShareLinkToken string `json:"share_link_token"`
}

// CreateClientRoom provisions a new client room (folder + share link
// bundle) owned by the authenticated workspace. Admin-only per the
// same reasoning as share-link / guest-invite creation: configuring
// external access is a sensitive operation.
func (h *Handler) CreateClientRoom(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createClientRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			http.Error(w, "invalid expires_at (expected RFC3339)", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}
	room, link, err := h.clientRooms.Create(r.Context(), workspaceID, userID, sharing.ClientRoomInput{
		Name:           req.Name,
		Password:       req.Password,
		ExpiresAt:      expiresAt,
		DropboxEnabled: req.DropboxEnabled,
	})
	if err != nil {
		if errors.Is(err, sharing.ErrInvalidRoomName) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, clientRoomResponse{ClientRoom: room, ShareLinkToken: link.Token})
}

// ListClientRooms returns every room in the workspace, newest first.
// Any authenticated workspace member may list; room contents are still
// gated by folder-level permissions downstream.
func (h *Handler) ListClientRooms(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	rooms, err := h.clientRooms.List(r.Context(), workspaceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

// GetClientRoom returns a single room. Scoped to workspace.
func (h *Handler) GetClientRoom(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	room, err := h.clientRooms.Get(r.Context(), workspaceID, id)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// DeleteClientRoom tears down a room (revoke share link + delete room
// row). Admin-only. The backing folder is intentionally not deleted;
// see ClientRoomService.Delete for rationale.
func (h *Handler) DeleteClientRoom(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.clientRooms.Delete(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Search handler -----------------------------------------------------------

// Search runs a workspace-scoped FTS query over file + folder names and
// returns results ranked by ts_rank_cd. q is required; limit defaults
// to DefaultLimit and is capped at MaxLimit in the service layer.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	if h.search == nil {
		http.Error(w, "search not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	limit := parseIntParam(r.URL.Query().Get("limit"), search.DefaultLimit)
	offset := parseIntParam(r.URL.Query().Get("offset"), 0)
	// Cap at the handler layer so the response envelope echoes the
	// clamped value back to the client. The service also caps
	// defensively, but echoing the pre-service limit would lie to
	// clients that paginate on len(results) < limit.
	if limit > search.MaxLimit {
		limit = search.MaxLimit
	}
	results, err := h.search.Search(r.Context(), workspaceID, query, limit, offset)
	if err != nil {
		if errors.Is(err, search.ErrInvalidQuery) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "search: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hits":   results,
		"query":  query,
		"limit":  limit,
		"offset": offset,
	})
}

// writeSharingError maps sharing-service errors to HTTP responses.
// ErrNotFound becomes 404, invalid-input errors become 400, expired /
// exhausted / password-related errors become 410 / 429 / 401 / 403 so
// clients can distinguish them without parsing the error text.
func writeSharingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sharing.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, sharing.ErrInvalidResourceType),
		errors.Is(err, sharing.ErrInvalidRole),
		errors.Is(err, sharing.ErrInvalidEmail):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, sharing.ErrLinkExpired),
		errors.Is(err, sharing.ErrInviteExpired):
		http.Error(w, err.Error(), http.StatusGone)
	case errors.Is(err, sharing.ErrLinkExhausted):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, sharing.ErrPasswordRequired):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, sharing.ErrPasswordIncorrect):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, sharing.ErrInviteAlreadyUsed):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		writeServiceError(w, err)
	}
}

// --- Notifications -------------------------------------------------------

// ListNotifications returns the caller's notifications (unread-first,
// newest-first). limit is capped at 100.
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		http.Error(w, "notifications not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	limit := parseIntParam(r.URL.Query().Get("limit"), 20)
	offset := parseIntParam(r.URL.Query().Get("offset"), 0)
	items, err := h.notifications.List(r.Context(), workspaceID, userID, limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notifications": items,
		"limit":         limit,
		"offset":        offset,
	})
}

// MarkNotificationRead flips a single notification to read.
func (h *Handler) MarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		http.Error(w, "notifications not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.notifications.MarkRead(r.Context(), workspaceID, userID, id); err != nil {
		if errors.Is(err, notification.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MarkAllNotificationsRead flips every unread notification for the caller.
func (h *Handler) MarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if h.notifications == nil {
		http.Error(w, "notifications not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	if err := h.notifications.MarkAllRead(r.Context(), workspaceID, userID); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Preview URL ---------------------------------------------------------

// PreviewURL returns a presigned GET URL for the latest generated
// preview of a file. Returns 404 when no preview has been built yet
// (either the mime type is unsupported or the worker hasn't run
// against this version); the frontend renders a placeholder icon in
// that case.
func (h *Handler) PreviewURL(w http.ResponseWriter, r *http.Request) {
	if h.previews == nil || h.storage == nil {
		http.Error(w, "previews not configured", http.StatusNotImplemented)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	p, err := h.previews.GetLatestByFile(r.Context(), id)
	if err != nil {
		if errors.Is(err, preview.ErrNotFound) {
			http.Error(w, "no preview available", http.StatusNotFound)
			return
		}
		writeServiceError(w, err)
		return
	}
	url, err := h.storage.GenerateDownloadURL(r.Context(), p.ObjectKey, 0)
	if err != nil {
		http.Error(w, "preview url: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"preview_url": url,
		"object_key":  p.ObjectKey,
		"mime_type":   p.MimeType,
	})
}
