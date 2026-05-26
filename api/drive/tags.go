package drive

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

type addTagRequest struct {
	Tag string `json:"tag"`
}

// AddFileTag attaches a single tag to a file. Editor permission on the
// file is required so viewers cannot pollute the tag space.
func (h *Handler) AddFileTag(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	var req addTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
		writeServiceError(w, r, err)
		return
	}
	t, err := h.files.AddTag(r.Context(), workspaceID, id, userID, req.Tag)
	if err != nil {
		writeTagError(w, r, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileTagAdd, permission.ResourceFile, id, map[string]any{
		"tag": t.Tag,
	})
	writeJSON(w, http.StatusCreated, t)
}

// ListFileTags returns every tag attached to a file. Viewer permission
// on the file is sufficient.
func (h *Handler) ListFileTags(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleViewer); err != nil {
		writeServiceError(w, r, err)
		return
	}
	tags, err := h.files.ListTags(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

// RemoveFileTag detaches a single tag. Editor permission required.
func (h *Handler) RemoveFileTag(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	tag := chi.URLParam(r, "tag")
	if tag == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "tag is required")
		return
	}
	// net/http already decodes Request.URL.Path before chi extracts
	// route params, so the value here is already URL-decoded. Tags
	// containing '/' or '%' are rejected at AddTag time, so the
	// path-param round-trip is unambiguous and no further unescaping
	// is required.
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
		writeServiceError(w, r, err)
		return
	}
	if err := h.files.RemoveTag(r.Context(), workspaceID, id, tag); err != nil {
		writeTagError(w, r, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileTagRemove, permission.ResourceFile, id, map[string]any{
		"tag": strings.ToLower(strings.TrimSpace(tag)),
	})
	w.WriteHeader(http.StatusNoContent)
}

// writeTagError maps the file-package tag errors to HTTP statuses.
func writeTagError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, file.ErrTagAlreadyExists):
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, err.Error())
	case errors.Is(err, file.ErrInvalidTag):
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
	default:
		writeServiceError(w, r, err)
	}
}
