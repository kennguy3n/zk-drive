package drive

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

// resourceTypeDocument is the activity_log.resource_type label for
// document events. Distinct from permission.ResourceFolder /
// permission.ResourceFile because documents are a separate resource
// family even though their permission inheritance walks through the
// parent folder. The permission service does NOT validate this
// value (documents are folder-inherited, not directly granted).
const resourceTypeDocument = "document"

// Document DTOs ------------------------------------------------------------
//
// Payloads are base64-encoded over the wire so the JSON layer stays
// printable (Yjs updates are binary). The wire format trades a ~33%
// size penalty against the structured-logging / SDK ergonomics
// benefit of keeping everything string-typed in the API contract.

type createDocumentRequest struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	FolderID    string `json:"folder_id"`
	Name        string `json:"name"`
	CollabMode  string `json:"collab_mode,omitempty"`
}

type renameDocumentRequest struct {
	Name string `json:"name"`
}

type setCollabModeRequest struct {
	CollabMode string `json:"collab_mode"`
}

type appendDeltaRequest struct {
	// Payload is base64-encoded binary Yjs update bytes.
	Payload string `json:"payload"`
}

type documentResponse struct {
	*document.Document
	EncryptionMode  string              `json:"encryption_mode"`
	Capability      document.Capability `json:"capability"`
	AllowedModes    []string            `json:"allowed_collab_modes"`
}

type snapshotResponse struct {
	Document     documentResponse `json:"document"`
	YState       string           `json:"y_state"`        // base64
	YStateVector string           `json:"y_state_vector"` // base64
	TailDeltas   []deltaResponse  `json:"tail_deltas"`
}

type deltaResponse struct {
	Seq          int64     `json:"seq"`
	Payload      string    `json:"payload"` // base64
	AuthorUserID uuid.UUID `json:"author_user_id"`
	CreatedAt    string    `json:"created_at"`
}

// CreateDocument creates a new document in a folder. The folder's
// encryption_mode determines which collab_modes are allowed; the
// service rejects out-of-policy combinations with a 422.
func (h *Handler) CreateDocument(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req createDocumentRequest
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
	folderID, err := uuid.Parse(req.FolderID)
	if err != nil {
		http.Error(w, "invalid folder_id", http.StatusBadRequest)
		return
	}
	// Documents inherit folder-level permissions: the caller must
	// have at least viewer access to the folder. Editor-level is
	// required to create — same as folders / files.
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, folderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	doc, parent, err := h.documents.Create(r.Context(), document.CreateInput{
		WorkspaceID: workspaceID,
		FolderID:    folderID,
		Name:        req.Name,
		CollabMode:  req.CollabMode,
		CreatedBy:   userID,
	})
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionDocumentCreate, resourceTypeDocument, doc.ID, map[string]any{
		"folder_id":   doc.FolderID,
		"name":        doc.Name,
		"collab_mode": doc.CollabMode,
	})
	writeJSON(w, http.StatusCreated, newDocumentResponse(doc, parent))
}

// GetDocument returns a document plus its current capability.
func (h *Handler) GetDocument(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	doc, parent, err := h.documents.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newDocumentResponse(doc, parent))
}

// ListFolderDocuments returns the documents under a folder.
func (h *Handler) ListFolderDocuments(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	folderID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid folder id", http.StatusBadRequest)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, folderID, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	parent, err := h.folders.GetByID(r.Context(), workspaceID, folderID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	docs, err := h.documents.ListByFolder(r.Context(), workspaceID, folderID)
	if err != nil {
		http.Error(w, "list documents: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]documentResponse, 0, len(docs))
	for _, d := range docs {
		out = append(out, newDocumentResponse(d, parent))
	}
	writeJSON(w, http.StatusOK, out)
}

// RenameDocument updates the document name.
func (h *Handler) RenameDocument(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	doc, _, err := h.documents.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	var req renameDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	oldName := doc.Name
	updated, err := h.documents.Rename(r.Context(), workspaceID, id, req.Name)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	parent, err := h.folders.GetByID(r.Context(), workspaceID, updated.FolderID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionDocumentRename, resourceTypeDocument, updated.ID, map[string]any{
		"folder_id": updated.FolderID,
		"old_name":  oldName,
		"new_name":  updated.Name,
	})
	writeJSON(w, http.StatusOK, newDocumentResponse(updated, parent))
}

// SetDocumentCollabMode changes the document's collab mode within
// the folder's capability ceiling.
func (h *Handler) SetDocumentCollabMode(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	doc, _, err := h.documents.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	var req setCollabModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	oldMode := doc.CollabMode
	updated, err := h.documents.SetCollabMode(r.Context(), workspaceID, id, req.CollabMode)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	parent, err := h.folders.GetByID(r.Context(), workspaceID, updated.FolderID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionDocumentChangeCollabMode, resourceTypeDocument, updated.ID, map[string]any{
		"folder_id":       updated.FolderID,
		"old_collab_mode": oldMode,
		"new_collab_mode": updated.CollabMode,
	})
	writeJSON(w, http.StatusOK, newDocumentResponse(updated, parent))
}

// DeleteDocument soft-deletes a document.
func (h *Handler) DeleteDocument(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	doc, _, err := h.documents.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.documents.Delete(r.Context(), workspaceID, id); err != nil {
		writeDocumentError(w, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionDocumentDelete, resourceTypeDocument, id, map[string]any{
		"folder_id": doc.FolderID,
		"name":      doc.Name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// GetDocumentSnapshot returns the snapshot bundle for a cold-opening
// client: y_state + tail deltas above the snapshot floor.
func (h *Handler) GetDocumentSnapshot(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	snap, err := h.documents.Snapshot(r.Context(), workspaceID, id)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, snap.Document.FolderID, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	tail := make([]deltaResponse, 0, len(snap.TailDeltas))
	for _, d := range snap.TailDeltas {
		tail = append(tail, deltaResponse{
			Seq:          d.Seq,
			Payload:      base64.StdEncoding.EncodeToString(d.Payload),
			AuthorUserID: d.AuthorUserID,
			CreatedAt:    d.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, snapshotResponse{
		Document:     newDocumentResponse(snap.Document, snap.Folder),
		YState:       base64.StdEncoding.EncodeToString(snap.Document.YState),
		YStateVector: base64.StdEncoding.EncodeToString(snap.Document.YStateVector),
		TailDeltas:   tail,
	})
}

// ListDocumentDeltas returns deltas above the supplied ?after_seq
// cursor. Used by clients that want to fast-forward without
// downloading the snapshot.
func (h *Handler) ListDocumentDeltas(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	doc, _, err := h.documents.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleViewer); err != nil {
		writeServiceError(w, err)
		return
	}
	afterSeq, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
	if afterSeq < 0 {
		afterSeq = 0
	}
	limit := parseIntParam(r.URL.Query().Get("limit"), 100)
	deltas, err := h.documents.ListDeltas(r.Context(), workspaceID, id, afterSeq, limit)
	if err != nil {
		http.Error(w, "list deltas: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]deltaResponse, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, deltaResponse{
			Seq:          d.Seq,
			Payload:      base64.StdEncoding.EncodeToString(d.Payload),
			AuthorUserID: d.AuthorUserID,
			CreatedAt:    d.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deltas":    out,
		"after_seq": afterSeq,
		"limit":     limit,
	})
}

// AppendDocumentDelta accepts a single Yjs update from a client and
// persists it. The HTTP path is intended for clients without a
// WebSocket connection (e.g. mobile in foreground-sync-only mode);
// the P2b WebSocket provider provides the low-latency duplex path.
func (h *Handler) AppendDocumentDelta(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil {
		http.Error(w, "documents disabled", http.StatusServiceUnavailable)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	doc, _, err := h.documents.GetByID(r.Context(), workspaceID, id)
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleEditor); err != nil {
		writeServiceError(w, err)
		return
	}
	var req appendDeltaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	payload, err := base64.StdEncoding.DecodeString(req.Payload)
	if err != nil {
		http.Error(w, "payload must be base64", http.StatusBadRequest)
		return
	}
	result, err := h.documents.AppendDelta(r.Context(), document.AppendDeltaInput{
		WorkspaceID:  workspaceID,
		DocumentID:   id,
		Payload:      payload,
		AuthorUserID: userID,
	})
	if err != nil {
		writeDocumentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"seq":             result.Delta.Seq,
		"created_at":      result.Delta.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		"compaction_due":  result.CompactionDue,
		"pending_deltas":  result.PendingDeltaCount,
	})
}

// newDocumentResponse bundles the document with its current folder-
// derived capability + the allowed-modes list so the frontend can
// gate its UI without a second round trip.
func newDocumentResponse(d *document.Document, parent *folder.Folder) documentResponse {
	mode := ""
	if parent != nil {
		mode = parent.EncryptionMode
	}
	return documentResponse{
		Document:       d,
		EncryptionMode: mode,
		Capability:     document.ResolveCapability(mode),
		AllowedModes:   document.AllowedCollabModesFor(mode),
	}
}

// writeDocumentError maps document-package errors to HTTP statuses
// then falls through to writeServiceError for any folder / file /
// permission errors that surface through the document service.
func writeDocumentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, document.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, document.ErrInvalidName),
		errors.Is(err, document.ErrInvalidCollabMode),
		errors.Is(err, document.ErrEmptyPayload):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, document.ErrCollabModeNotAllowed):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, document.ErrPayloadTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, document.ErrSeqConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		writeServiceError(w, err)
	}
}
