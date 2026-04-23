package drive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// Handler serves workspace / folder / file HTTP endpoints.
type Handler struct {
	pool       *pgxpool.Pool
	workspaces *workspace.Service
	folders    *folder.Service
	files      *file.Service
	users      *user.Service
}

// NewHandler constructs a Handler from the underlying services. The pool is
// used to run multi-step writes (e.g. CreateWorkspace) atomically.
func NewHandler(pool *pgxpool.Pool, ws *workspace.Service, fs *folder.Service, fl *file.Service, us *user.Service) *Handler {
	return &Handler{pool: pool, workspaces: ws, folders: fs, files: fl, users: us}
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
	f, err := h.folders.Rename(r.Context(), workspaceID, id, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
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
	if err := h.folders.Delete(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
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
	f, err := h.folders.Move(r.Context(), workspaceID, id, parentID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
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
	f, err := h.files.Create(r.Context(), workspaceID, folderID, req.Name, req.MimeType, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
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
	f, err := h.files.Rename(r.Context(), workspaceID, id, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
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
	if err := h.files.Delete(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
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
	f, err := h.files.Move(r.Context(), workspaceID, id, folderID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
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
	versions, err := h.files.ListVersions(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
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
	case errors.Is(err, folder.ErrNotFound), errors.Is(err, file.ErrNotFound), errors.Is(err, workspace.ErrNotFound), errors.Is(err, user.ErrNotFound):
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
