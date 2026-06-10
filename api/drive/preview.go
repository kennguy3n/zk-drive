package drive

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/scan"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

// PreviewURL returns a presigned GET URL for the latest generated
// preview of a file. Returns 404 when no preview has been built yet
// (either the mime type is unsupported or the worker hasn't run
// against this version); the frontend renders a placeholder icon in
// that case.
func (h *Handler) PreviewURL(w http.ResponseWriter, r *http.Request) {
	if h.previews == nil || (h.storage == nil && h.storageFactory == nil) {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "previews not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	// Bind the file id to the caller's workspace before any permission
	// logic runs. Admins bypass assertResourceAccess, so without this
	// lookup an admin in workspace A could request a file id from
	// workspace B and get a presigned URL to B's preview.
	f, err := h.files.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleViewer); err != nil {
		writeServiceError(w, r, err)
		return
	}
	// Refuse to serve a preview for a quarantined version for the
	// same reason DownloadURL refuses: the preview is derived from
	// the infected source bytes and we do not want to surface it in
	// the UI. We check both the file's current version *and* the
	// specific version the preview was generated from: GetLatestByFile
	// returns the newest preview by created_at, which may predate the
	// current version if the preview worker has not caught up.
	if f.CurrentVersionID != nil {
		current, verr := h.files.GetVersionByID(r.Context(), workspaceID, *f.CurrentVersionID)
		if verr != nil && !errors.Is(verr, file.ErrNotFound) {
			writeServiceError(w, r, verr)
			return
		}
		if current != nil && current.ScanStatus == scan.StatusQuarantined {
			middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeVirusDetected, "file version quarantined by virus scan")
			return
		}
	}
	p, err := h.previews.GetLatestByFile(r.Context(), id)
	if err != nil {
		if errors.Is(err, preview.ErrNotFound) {
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "no preview available")
			return
		}
		writeServiceError(w, r, err)
		return
	}
	previewVersion, verr := h.files.GetVersionByID(r.Context(), workspaceID, p.VersionID)
	if verr != nil && !errors.Is(verr, file.ErrNotFound) {
		writeServiceError(w, r, verr)
		return
	}
	if previewVersion != nil && previewVersion.ScanStatus == scan.StatusQuarantined {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeVirusDetected, "file version quarantined by virus scan")
		return
	}
	store := h.resolveStorage(r.Context(), workspaceID)
	if store == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "storage not configured")
		return
	}
	url, err := store.GenerateDownloadURL(r.Context(), p.ObjectKey, storage.DefaultPresignExpiry)
	if err != nil {
		middleware.RespondInternalError(w, r, "preview url", err)
		return
	}
	// Previews are immutable per (file, version) and re-fetched often
	// as the user scrolls a grid, so the browser-cache win here is even
	// larger than for downloads. Same private + sub-expiry policy.
	setPresignedURLCacheControl(w, storage.DefaultPresignExpiry)
	writeJSON(w, http.StatusOK, map[string]any{
		"preview_url": url,
		"object_key":  p.ObjectKey,
		"mime_type":   p.MimeType,
	})
}
