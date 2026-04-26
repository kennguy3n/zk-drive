// Package kchat exposes the HTTP surface for KChat ↔ ZK Drive
// integration: room-folder mapping, permission sync, and attachment
// upload helpers. Routes mount under /api/kchat in cmd/server/main.go
// and tests/integration/setup_test.go.
package kchat

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	kchatpkg "github.com/kennguy3n/zk-drive/internal/kchat"
)

// Handler serves /api/kchat/* endpoints. The handler is intentionally
// thin — it delegates to kchat.RoomService and translates service
// errors into HTTP status codes.
type Handler struct {
	rooms *kchatpkg.RoomService
}

// NewHandler returns a new Handler backed by the given RoomService.
func NewHandler(rooms *kchatpkg.RoomService) *Handler {
	return &Handler{rooms: rooms}
}

// RegisterRoutes mounts every route the handler serves on r. The
// caller is responsible for wiring auth + tenant-guard middleware
// outside this method so the package stays decoupled from auth
// internals.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/rooms", h.CreateRoom)
	r.Get("/rooms", h.ListRooms)
	r.Get("/rooms/{id}", h.GetRoom)
	r.Delete("/rooms/{id}", h.DeleteRoom)
	r.Post("/rooms/{id}/sync-members", h.SyncMembers)
	r.Post("/attachments/upload-url", h.AttachmentUploadURL)
	r.Post("/attachments/confirm", h.AttachmentConfirm)
}

// createRoomRequest is the JSON body accepted by POST /rooms.
type createRoomRequest struct {
	KChatRoomID string `json:"kchat_room_id"`
}

// CreateRoom maps a KChat room id to a freshly-provisioned folder.
// Responds 409 when the room is already mapped so a retried POST
// surfaces the duplicate rather than silently no-op'ing.
func (h *Handler) CreateRoom(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	room, err := h.rooms.CreateRoomFolder(r.Context(), workspaceID, req.KChatRoomID, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, room)
}

// ListRooms returns every mapping in the workspace.
func (h *Handler) ListRooms(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	rooms, err := h.rooms.List(r.Context(), workspaceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if rooms == nil {
		rooms = []*kchatpkg.RoomFolder{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

// GetRoom returns a single mapping by id.
func (h *Handler) GetRoom(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	room, err := h.rooms.Get(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// DeleteRoom removes the mapping row. The backing folder is left
// intact so operators can keep the uploaded files; deleting the
// folder is a separate action through the regular folder API.
func (h *Handler) DeleteRoom(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.rooms.Delete(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// syncMembersRequest is the JSON body accepted by
// POST /rooms/{id}/sync-members.
type syncMembersRequest struct {
	Members []memberSyncJSON `json:"members"`
}

type memberSyncJSON struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// SyncMembers reconciles the supplied member list against the
// folder's existing user grants. Adds new grants, revokes removed
// ones, and updates roles where they differ.
func (h *Handler) SyncMembers(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	mapping, err := h.rooms.Get(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var req syncMembersRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	members := make([]kchatpkg.MemberSync, 0, len(req.Members))
	for _, m := range req.Members {
		uid, err := uuid.Parse(m.UserID)
		if err != nil {
			http.Error(w, "invalid user_id: "+m.UserID, http.StatusBadRequest)
			return
		}
		members = append(members, kchatpkg.MemberSync{UserID: uid, Role: m.Role})
	}
	if err := h.rooms.SyncMembers(r.Context(), workspaceID, mapping.KChatRoomID, members); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"synced": len(members)})
}

// attachmentUploadRequest is the JSON body accepted by
// POST /attachments/upload-url.
type attachmentUploadRequest struct {
	KChatRoomID string `json:"kchat_room_id"`
	Filename    string `json:"filename"`
	MimeType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// AttachmentUploadURL resolves the room's folder, creates the file
// metadata row, and returns a presigned PUT URL.
func (h *Handler) AttachmentUploadURL(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req attachmentUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	res, err := h.rooms.AttachmentUploadURL(r.Context(), workspaceID, req.KChatRoomID, req.Filename, req.MimeType, req.SizeBytes, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// attachmentConfirmRequest is the JSON body accepted by
// POST /attachments/confirm.
type attachmentConfirmRequest struct {
	FileID    string `json:"file_id"`
	ObjectKey string `json:"object_key"`
	Checksum  string `json:"checksum"`
	SizeBytes int64  `json:"size_bytes"`
}

// AttachmentConfirm promotes a previously-minted upload URL into a
// FileVersion. Mirrors the regular ConfirmUpload contract.
func (h *Handler) AttachmentConfirm(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req attachmentConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	fileID, err := uuid.Parse(req.FileID)
	if err != nil {
		http.Error(w, "invalid file_id", http.StatusBadRequest)
		return
	}
	res, err := h.rooms.ConfirmAttachment(r.Context(), workspaceID, fileID, req.ObjectKey, req.Checksum, req.SizeBytes, userID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeJSON encodes payload to w with the given status code. Cloned
// from api/drive/handler.go so the kchat package stays free of
// internal cross-package helpers.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// writeServiceError translates kchat / underlying service errors
// into HTTP status codes. Unknown errors map to 500.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, kchatpkg.ErrRoomNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, kchatpkg.ErrRoomAlreadyMapped):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, kchatpkg.ErrInvalidRoomID),
		errors.Is(err, kchatpkg.ErrInvalidRole):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
