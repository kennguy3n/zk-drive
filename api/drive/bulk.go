package drive

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/scan"
	"github.com/kennguy3n/zk-drive/internal/storage"
)

// MaxBulkItems caps the number of resources (files + folders) a single
// bulk request may touch. Intentionally small so the handler stays
// within a single HTTP request budget even when permission checks are
// sequential.
const MaxBulkItems = 100

// MaxBulkDownloadBytes caps the total uncompressed byte count a single
// zip-download request will stream. Protects the API server from
// accidental TB-scale requests.
const MaxBulkDownloadBytes = 1 << 30 // 1 GiB

type bulkMutateRequest struct {
	FileIDs        []string `json:"file_ids,omitempty"`
	FolderIDs      []string `json:"folder_ids,omitempty"`
	TargetFolderID string   `json:"target_folder_id,omitempty"`
}

type bulkDownloadRequest struct {
	FileIDs []string `json:"file_ids"`
}

type bulkFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

type bulkResponse struct {
	Succeeded []string      `json:"succeeded"`
	Failed    []bulkFailure `json:"failed"`
}

func (bulkResponse) itemCount(req bulkMutateRequest) int {
	return len(req.FileIDs) + len(req.FolderIDs)
}

// BulkMove relocates a set of files and folders into a target folder.
// Each item is checked independently; failures on one item do not
// abort the rest. Returns a per-item success/failure summary.
func (h *Handler) BulkMove(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	req, ok := decodeBulkMutate(w, r, true)
	if !ok {
		return
	}
	targetID, err := uuid.Parse(req.TargetFolderID)
	if err != nil {
		http.Error(w, "invalid target_folder_id", http.StatusBadRequest)
		return
	}
	if _, err := h.folders.GetByID(r.Context(), workspaceID, targetID); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, targetID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}

	resp := bulkResponse{Succeeded: []string{}, Failed: []bulkFailure{}}
	for _, raw := range req.FileIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: "invalid uuid"})
			continue
		}
		if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		if _, err := h.files.Move(r.Context(), workspaceID, id, targetID); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		h.logActivity(r.Context(), activity.ActionFileBulkMove, permission.ResourceFile, id, map[string]any{"target": targetID})
		resp.Succeeded = append(resp.Succeeded, raw)
	}
	for _, raw := range req.FolderIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: "invalid uuid"})
			continue
		}
		if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, id, permission.RoleEditor); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		if _, err := h.folders.Move(r.Context(), workspaceID, id, &targetID); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		h.logActivity(r.Context(), activity.ActionFolderMove, permission.ResourceFolder, id, map[string]any{"target": targetID})
		resp.Succeeded = append(resp.Succeeded, raw)
	}
	writeJSON(w, http.StatusOK, resp)
}

// BulkDelete soft-deletes a set of files and folders.
func (h *Handler) BulkDelete(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	req, ok := decodeBulkMutate(w, r, false)
	if !ok {
		return
	}
	resp := bulkResponse{Succeeded: []string{}, Failed: []bulkFailure{}}
	for _, raw := range req.FileIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: "invalid uuid"})
			continue
		}
		if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleEditor); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		if err := h.files.Delete(r.Context(), workspaceID, id); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		h.logActivity(r.Context(), activity.ActionFileBulkDelete, permission.ResourceFile, id, nil)
		resp.Succeeded = append(resp.Succeeded, raw)
	}
	for _, raw := range req.FolderIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: "invalid uuid"})
			continue
		}
		if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, id, permission.RoleEditor); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		if err := h.folders.Delete(r.Context(), workspaceID, id); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		h.logActivity(r.Context(), activity.ActionFolderDelete, permission.ResourceFolder, id, nil)
		resp.Succeeded = append(resp.Succeeded, raw)
	}
	writeJSON(w, http.StatusOK, resp)
}

// BulkCopy creates new file metadata rows pointing to the existing
// version objects. Folder copy is not supported (nested hierarchies
// make the permission/storage-cost accounting too subtle for a first
// pass).
func (h *Handler) BulkCopy(w http.ResponseWriter, r *http.Request) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	req, ok := decodeBulkMutate(w, r, true)
	if !ok {
		return
	}
	if len(req.FolderIDs) > 0 {
		http.Error(w, "folder copy is not supported; use file_ids only", http.StatusBadRequest)
		return
	}
	targetID, err := uuid.Parse(req.TargetFolderID)
	if err != nil {
		http.Error(w, "invalid target_folder_id", http.StatusBadRequest)
		return
	}
	if _, err := h.folders.GetByID(r.Context(), workspaceID, targetID); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, targetID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	resp := bulkResponse{Succeeded: []string{}, Failed: []bulkFailure{}}
	for _, raw := range req.FileIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: "invalid uuid"})
			continue
		}
		if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleViewer); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		src, err := h.files.GetByID(r.Context(), workspaceID, id)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		if src.CurrentVersionID == nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: "file has no current version"})
			continue
		}
		srcVer, err := h.files.GetVersionByID(r.Context(), workspaceID, *src.CurrentVersionID)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		if srcVer.ScanStatus == scan.StatusQuarantined {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: "file version quarantined by virus scan"})
			continue
		}
		if err := h.billing.CheckStorageQuota(r.Context(), workspaceID, srcVer.SizeBytes); err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		newFile, err := h.files.Create(r.Context(), workspaceID, targetID, src.Name, src.MimeType, userID)
		if err != nil {
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		newVersion := &file.FileVersion{
			ID:         uuid.New(),
			FileID:     newFile.ID,
			ObjectKey:  srcVer.ObjectKey,
			SizeBytes:  srcVer.SizeBytes,
			Checksum:   srcVer.Checksum,
			CreatedBy:  userID,
		}
		if err := h.files.ConfirmVersion(r.Context(), workspaceID, newVersion); err != nil {
			// Soft-delete the orphan file row we just created so the
			// target folder doesn't accumulate 0-byte rows when the
			// version write fails. The Delete error is swallowed: if
			// it also fails, the original ConfirmVersion error is the
			// more useful one to surface, and an admin can clean up
			// the orphan later.
			_ = h.files.Delete(r.Context(), workspaceID, newFile.ID)
			resp.Failed = append(resp.Failed, bulkFailure{ID: raw, Error: err.Error()})
			continue
		}
		h.billing.RecordUpload(r.Context(), workspaceID, srcVer.SizeBytes)
		h.logActivity(r.Context(), activity.ActionFileBulkCopy, permission.ResourceFile, newFile.ID, map[string]any{
			"source_file_id": src.ID,
		})
		resp.Succeeded = append(resp.Succeeded, newFile.ID.String())
	}
	writeJSON(w, http.StatusOK, resp)
}

// BulkDownload streams a zip archive of the requested files. This is
// one of the few endpoints where the API server proxies object bytes
// — the tradeoff is documented in docs/PROGRESS.md. Larger downloads
// should move to an async "build zip in object storage" worker later.
func (h *Handler) BulkDownload(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		http.Error(w, "storage not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	var req bulkDownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if len(req.FileIDs) == 0 {
		http.Error(w, "file_ids required", http.StatusBadRequest)
		return
	}
	if len(req.FileIDs) > MaxBulkItems {
		http.Error(w, fmt.Sprintf("max %d files per bulk download", MaxBulkItems), http.StatusBadRequest)
		return
	}
	// Resolve each file + permission + current version up front so we
	// can fail fast before opening the response stream. Once we start
	// writing the zip body we can't set a non-200 status.
	type prepped struct {
		name      string
		objectKey string
		size      int64
	}
	items := make([]prepped, 0, len(req.FileIDs))
	seenNames := make(map[string]int, len(req.FileIDs))
	var total int64
	for _, raw := range req.FileIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "invalid uuid: "+raw, http.StatusBadRequest)
			return
		}
		if err := h.assertResourceAccess(r.Context(), permission.ResourceFile, id, permission.RoleViewer); err != nil {
			writeServiceError(w, err)
			return
		}
		f, err := h.files.GetByID(r.Context(), workspaceID, id)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if f.CurrentVersionID == nil {
			http.Error(w, "file has no current version: "+raw, http.StatusConflict)
			return
		}
		v, err := h.files.GetVersionByID(r.Context(), workspaceID, *f.CurrentVersionID)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if v.ScanStatus == scan.StatusQuarantined {
			http.Error(w, "file version quarantined by virus scan: "+raw, http.StatusForbidden)
			return
		}
		total += v.SizeBytes
		if total > MaxBulkDownloadBytes {
			http.Error(w, fmt.Sprintf("bulk download exceeds %d byte cap", MaxBulkDownloadBytes), http.StatusRequestEntityTooLarge)
			return
		}
		items = append(items, prepped{name: dedupeZipName(seenNames, f.Name), objectKey: v.ObjectKey, size: v.SizeBytes})
	}
	if err := h.billing.CheckBandwidthQuota(r.Context(), workspaceID, total); err != nil {
		writeBillingError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="download.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	client := &http.Client{}
	for _, it := range items {
		if err := appendZipEntry(r.Context(), zw, client, h.storage, it.name, it.objectKey); err != nil {
			// Past the header we can only signal via the zip stream;
			// return without a partial entry and log server-side so
			// operators can correlate truncated downloads with a cause.
			log.Printf("drive: bulk download: append zip entry %q: %v", it.name, err)
			return
		}
	}
	h.billing.RecordDownload(r.Context(), workspaceID, total)
	h.logActivity(r.Context(), activity.ActionFileBulkDownload, permission.ResourceFile, uuid.Nil, map[string]any{
		"file_count":  len(items),
		"total_bytes": total,
	})
}

// dedupeZipName disambiguates zip entry names that would collide. The
// first occurrence of a name is used verbatim; subsequent collisions
// are suffixed before the extension (e.g. "report.pdf" -> "report
// (1).pdf"). This prevents most extractors from silently overwriting
// earlier entries when the user selects multiple files that happen to
// share a name across folders.
func dedupeZipName(seen map[string]int, name string) string {
	if _, ok := seen[name]; !ok {
		seen[name] = 1
		return name
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for {
		n := seen[name]
		candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		seen[name] = n + 1
		if _, taken := seen[candidate]; !taken {
			seen[candidate] = 1
			return candidate
		}
	}
}

func appendZipEntry(ctx context.Context, zw *zip.Writer, client *http.Client, st *storage.Client, name, objectKey string) error {
	url, err := st.GenerateDownloadURL(ctx, objectKey, storage.DefaultPresignExpiry)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("object fetch: %s", resp.Status)
	}
	entry, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, resp.Body)
	return err
}

func decodeBulkMutate(w http.ResponseWriter, r *http.Request, requireTarget bool) (bulkMutateRequest, bool) {
	var req bulkMutateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return req, false
	}
	if len(req.FileIDs)+len(req.FolderIDs) == 0 {
		http.Error(w, "file_ids or folder_ids required", http.StatusBadRequest)
		return req, false
	}
	if len(req.FileIDs)+len(req.FolderIDs) > MaxBulkItems {
		http.Error(w, fmt.Sprintf("max %d items per bulk request", MaxBulkItems), http.StatusBadRequest)
		return req, false
	}
	if requireTarget && req.TargetFolderID == "" {
		http.Error(w, "target_folder_id required", http.StatusBadRequest)
		return req, false
	}
	return req, true
}
