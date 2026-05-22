package drive

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/scan"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

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

// UploadURL generates a presigned PUT URL that lets the caller upload a
// single file version directly to zk-object-fabric. It creates the file
// metadata row up front so the client can reference a stable file ID when
// it later calls ConfirmUpload. The returned object_key is opaque to the
// client; it must be echoed back verbatim on confirm so the server records
// the exact key it signed.
func (h *Handler) UploadURL(w http.ResponseWriter, r *http.Request) {
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

	// Storage quota is enforced before we mint the presigned URL —
	// otherwise the client could PUT bytes we'd then refuse to confirm.
	// We can't know the exact upload size in advance, but checking
	// against current usage gives a good gate; the confirm-upload
	// step will record the actual byte count.
	if err := h.billing.CheckStorageQuota(r.Context(), workspaceID, 0); err != nil {
		writeBillingError(w, err)
		return
	}

	// Resolve the per-workspace storage client BEFORE creating the
	// metadata row. If storage is unconfigured for this workspace we
	// must reject the request without leaving an orphan file row that
	// has no corresponding upload URL or object.
	store := h.resolveStorage(r.Context(), workspaceID)
	if store == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}

	f, err := h.files.Create(r.Context(), workspaceID, folderID, req.Filename, req.MimeType, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	versionID := uuid.New()
	objectKey := storage.NewObjectKey(workspaceID, f.ID, versionID)

	url, err := store.GenerateUploadURL(r.Context(), objectKey, req.MimeType, storage.DefaultPresignExpiry)
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
	if h.storage == nil && h.storageFactory == nil {
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

	// The object_key is client-supplied; validate it matches the
	// canonical `<workspace>/<file>/<version>` form generated by
	// storage.NewObjectKey and is scoped to the caller's workspace
	// and file. A simple HasPrefix check is insufficient — a key like
	// "<workspace>/<file>/../../other-tenant/secret" would satisfy
	// the prefix but allow path traversal through whatever the
	// gateway's URL canonicalisation tolerates. ValidateObjectKey
	// enforces the full canonical shape (three UUIDs, strict
	// equality on the leading two, no traversal chars, no NUL,
	// no backslashes).
	versionID, err := storage.ValidateObjectKey(req.ObjectKey, workspaceID, f.ID)
	if err != nil {
		http.Error(w, "object_key does not belong to this file", http.StatusForbidden)
		return
	}

	// Replay detection: if a row with this versionID already exists
	// in file_versions for this file, a previous confirm has
	// committed it atomically and req.SizeBytes is already part of
	// (or has been superseded in) the workspace storage total
	// (GetStorageUsed = SUM(files.size_bytes), see
	// internal/billing/repository.go). Re-running CheckStorageQuota
	// with req.SizeBytes would double-count and 402-reject an
	// otherwise successful idempotent retry near the quota
	// boundary.
	//
	// We probe file_versions directly (rather than comparing
	// f.CurrentVersionID == versionID) so detection stays correct
	// when a newer V2 has advanced the file pointer past V1: a
	// stale V1 retry must still bypass the quota check because
	// V1's bytes are *not* net-new — V1 was already counted at its
	// original confirm, then superseded by V2. The narrower
	// "current pointer matches" heuristic would miss this case and
	// let CheckStorageQuota run, computing a phantom over-quota
	// that violates the idempotency contract. (Today no API in
	// this codebase produces a V2-supersedes-V1 on the same file
	// row, but the repository-level `if !fresh` guard already
	// handles it correctly, so detection-side correctness here
	// closes the matched gap.)
	//
	// On the replay path the subsequent ConfirmVersion hits its
	// ON CONFLICT branch and returns fresh=false, so the
	// side-effect gate further down also stays inert. The replay
	// branch only skips the quota check itself; it does *not*
	// skip ConfirmVersion (which still validates the identity
	// tuple via re-fetch and returns ErrVersionConflict on
	// mismatch).
	existingVersion, vErr := h.files.GetVersionByID(r.Context(), workspaceID, versionID)
	var isReplay bool
	switch {
	case vErr == nil:
		// A workspace-scoped version row exists. Only treat it as
		// a replay of *this* confirm if the row belongs to the
		// same file — a version id colliding with a row owned by
		// a different file in the same workspace is an identity
		// mismatch that ConfirmVersion will reject with
		// ErrVersionConflict, and for that path we want the quota
		// check to run normally (the request is not actually a
		// no-op retry of a prior committed confirm).
		isReplay = existingVersion.FileID == f.ID
	case errors.Is(vErr, file.ErrNotFound):
		isReplay = false
	default:
		// Unexpected DB error during replay probe. Surfacing 500
		// rather than silently proceeding so the caller can retry
		// idempotently on a transient blip.
		writeServiceError(w, vErr)
		return
	}

	if !isReplay {
		// Storage quota is re-checked against the actual size now that the
		// client has uploaded; the UploadURL pre-check only screens
		// already-over-quota workspaces. The S3 object is already written
		// here, but rejecting the confirm leaves the row unconfirmed and
		// the orphan object can be reclaimed by a future GC pass — better
		// than silently allowing unbounded overage.
		if err := h.billing.CheckStorageQuota(r.Context(), workspaceID, req.SizeBytes); err != nil {
			writeBillingError(w, err)
			return
		}
	}

	// Pin the version row's primary key to the UUID embedded in the
	// object_key the client just confirmed. UploadURL generated the
	// versionID, signed it into the S3 key, and handed both back to
	// the client; storing the same UUID in file_versions.id keeps
	// the database row, the S3 object, and any audit / activity log
	// entries referring to "version_id" in lock-step. Without this,
	// insertVersionTx would mint a fresh uuid.New(), creating a
	// permanent mismatch between the object key's version segment
	// and the DB row id — harmless today (downloads use the stored
	// object_key string) but a real source of confusion in audit
	// logs and any future code that round-trips through versionID.
	v := &file.FileVersion{
		ID:        versionID,
		FileID:    f.ID,
		ObjectKey: req.ObjectKey,
		SizeBytes: req.SizeBytes,
		Checksum:  req.Checksum,
		CreatedBy: userID,
	}
	fresh, err := h.files.ConfirmVersion(r.Context(), workspaceID, v)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	updated, err := h.files.GetByID(r.Context(), workspaceID, f.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	// Side effects (activity log, billing usage event, post-upload
	// job dispatch) MUST only run on the first successful confirm
	// for this version, never on an idempotent retry — otherwise
	// the audit timeline and billing ledger double-count network
	// hiccups, and the preview / scan / index workers re-process
	// the same object. ConfirmVersion returns `fresh=true` exactly
	// when the underlying INSERT actually created a new row; the
	// ON CONFLICT branch returns false. See
	// internal/file/repository.go's insertVersionTx for the full
	// rationale.
	if fresh {
		h.logActivity(r.Context(), activity.ActionFileUpload, permission.ResourceFile, f.ID, map[string]any{
			"version_id": v.ID,
			"size_bytes": v.SizeBytes,
		})
		h.billing.RecordUpload(r.Context(), workspaceID, v.SizeBytes)
		// Fan out post-upload work (preview, scan, index) via JetStream.
		// All three publishers are nil-safe so the handler behaves
		// identically when NATS is not configured locally. Publish errors
		// are logged and ignored so a flaky broker never fails an
		// otherwise-successful upload — workers can be re-triggered later.
		h.publishPostUploadJobs(r.Context(), f.ID, v.ID)
	}
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
	if h.storage == nil && h.storageFactory == nil {
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
	current, err := h.files.GetVersionByID(r.Context(), workspaceID, *f.CurrentVersionID)
	if err != nil {
		if errors.Is(err, file.ErrNotFound) {
			http.Error(w, "current version not found", http.StatusNotFound)
			return
		}
		writeServiceError(w, err)
		return
	}
	// Refuse to mint a presigned URL for a version clamd has already
	// flagged. Migration 008 pairs this check with the scan worker:
	// without it the scan pipeline only surfaces quarantine via the
	// admin notification, while the infected bytes remain pullable.
	if current.ScanStatus == scan.StatusQuarantined {
		http.Error(w, "file version quarantined by virus scan", http.StatusForbidden)
		return
	}
	if err := h.billing.CheckBandwidthQuota(r.Context(), workspaceID, current.SizeBytes); err != nil {
		writeBillingError(w, err)
		return
	}
	store := h.resolveStorage(r.Context(), workspaceID)
	if store == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	url, err := store.GenerateDownloadURL(r.Context(), current.ObjectKey, storage.DefaultPresignExpiry)
	if err != nil {
		http.Error(w, "generate download url: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Bandwidth accounting: record the version's size as a download.
	// We assume the client will fetch the full object (the typical
	// case for direct-to-storage downloads); ranged GETs are not
	// metered separately since the presigned URL is opaque.
	h.billing.RecordDownload(r.Context(), workspaceID, current.SizeBytes)
	h.logActivity(r.Context(), activity.ActionFileDownload, permission.ResourceFile, f.ID, map[string]any{
		"version_id": current.ID,
	})
	writeJSON(w, http.StatusOK, downloadURLResponse{
		DownloadURL: url,
		ObjectKey:   current.ObjectKey,
	})
}

// writeBillingError maps billing.ErrQuotaExceeded to 402 Payment
// Required so the frontend can prompt the user to upgrade.
func writeBillingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrQuotaExceeded):
		http.Error(w, err.Error(), http.StatusPaymentRequired)
	default:
		writeServiceError(w, err)
	}
}
