package drive

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

// Folder DTOs ---------------------------------------------------------------

type createFolderRequest struct {
	WorkspaceID    string  `json:"workspace_id"`
	ParentFolderID *string `json:"parent_folder_id,omitempty"`
	Name           string  `json:"name"`
	EncryptionMode string  `json:"encryption_mode,omitempty"`
}

type renameFolderRequest struct {
	Name string `json:"name"`
}

type moveFolderRequest struct {
	NewParentFolderID *string `json:"new_parent_folder_id,omitempty"`
}

// CreateFolder creates a new folder scoped to the authenticated workspace.
func (h *Handler) CreateFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	var req createFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.WorkspaceID != "" {
		reqWS, err := uuid.Parse(req.WorkspaceID)
		if err != nil || reqWS != workspaceID {
			middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeWrongTenant, "workspace_id mismatch")
			return
		}
	}
	var parentID *uuid.UUID
	if req.ParentFolderID != nil && *req.ParentFolderID != "" {
		pid, err := uuid.Parse(*req.ParentFolderID)
		if err != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid parent_folder_id")
			return
		}
		parentID = &pid
	}
	f, err := h.folders.CreateWithMode(r.Context(), workspaceID, parentID, req.Name, req.EncryptionMode, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFolderCreate, permission.ResourceFolder, f.ID, map[string]any{
		"name":             f.Name,
		"parent_folder_id": f.ParentFolderID,
		"encryption_mode":  f.EncryptionMode,
	})
	writeJSON(w, http.StatusCreated, f)
}

// GetFolder returns folder details plus child folders and files.
func (h *Handler) GetFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
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
		middleware.RespondInternalError(w, r, "list children", err)
		return
	}
	fileList, err := h.files.ListByFolder(r.Context(), workspaceID, id)
	if err != nil {
		middleware.RespondInternalError(w, r, "list files", err)
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
			middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeWrongTenant, "workspace_id mismatch")
			return
		}
	}
	parentParam := r.URL.Query().Get("parent_folder_id")
	var parentID *uuid.UUID
	if parentParam != "" && parentParam != "root" {
		pid, err := uuid.Parse(parentParam)
		if err != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid parent_folder_id")
			return
		}
		parentID = &pid
	}
	list, err := h.folders.ListChildren(r.Context(), workspaceID, parentID)
	if err != nil {
		middleware.RespondInternalError(w, r, "list folders", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": list})
}

// RenameFolder updates a folder's name in-place.
func (h *Handler) RenameFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	var req renameFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
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
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, id, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	// Snapshot every file AND collab document in the subtree BEFORE
	// the recursive folder soft-delete cascades to their deleted_at
	// columns. Once SoftDeleteSubtree commits, the repo-level list
	// queries filter out deleted_at IS NOT NULL rows so the emit
	// phase would silently miss them. Symmetric with the single-
	// resource Delete paths so subscribers (webhooks, changefeed,
	// activity log) see deleted events regardless of whether the
	// resource was removed individually or via folder cascade.
	fileSnaps := h.snapshotFilesForFolderSubtreeDelete(r.Context(), workspaceID, id)
	docSnaps := h.snapshotDocumentsForFolderSubtreeDelete(r.Context(), workspaceID, id)
	if err := h.folders.Delete(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFolderDelete, permission.ResourceFolder, id, nil)
	// Emit one ActionDocumentDelete per cascaded document so the
	// changefeed and desktop sync clients see each removal — a
	// single folder.delete event isn't enough because the document
	// kind has its own changefeed stream that downstream consumers
	// filter on. Same TOCTOU-tolerance contract as the file path
	// (snapshot → delete → emit, best-effort relative to the
	// durable folder soft-delete which already committed).
	h.emitFolderCascadeDocumentDeletes(r.Context(), docSnaps)
	h.emitWebhookFileDeletedBatch(r.Context(), workspaceID, fileSnaps)
	w.WriteHeader(http.StatusNoContent)
}

// MoveFolder relocates a folder under a new parent.
func (h *Handler) MoveFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	var req moveFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	var parentID *uuid.UUID
	if req.NewParentFolderID != nil && *req.NewParentFolderID != "" {
		pid, perr := uuid.Parse(*req.NewParentFolderID)
		if perr != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid new_parent_folder_id")
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
