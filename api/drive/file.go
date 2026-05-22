package drive

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

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
	dstFolder, err := h.folders.GetByID(r.Context(), workspaceID, folderID)
	if err != nil {
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
	// Cross-mode moves require re-upload because the underlying objects
	// live under different keying / placement regimes; reject the
	// metadata-level move with 409 Conflict before touching files.Move.
	srcFile, err := h.files.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	srcFolder, err := h.folders.GetByID(r.Context(), workspaceID, srcFile.FolderID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !sameFolderEncryptionMode(srcFolder.EncryptionMode, dstFolder.EncryptionMode) {
		writeServiceError(w, folder.ErrEncryptionModeMismatch)
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
