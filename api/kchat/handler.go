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
	"github.com/kennguy3n/zk-drive/internal/ai"
	"github.com/kennguy3n/zk-drive/internal/file"
	kchatpkg "github.com/kennguy3n/zk-drive/internal/kchat"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// requireAdmin enforces that the caller has the admin role. Returns
// true when the request was already responded to (403 written) and
// the caller should bail out. Mirrors the per-handler check used
// throughout api/drive/handler.go for mutation endpoints.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeAdminOnly, "admin role required")
		return true
	}
	return false
}

// Handler serves /api/kchat/* endpoints. The handler is intentionally
// thin — it delegates to kchat.RoomService and translates service
// errors into HTTP status codes.
type Handler struct {
	rooms   *kchatpkg.RoomService
	summary *ai.SummaryService
}

// NewHandler returns a new Handler backed by the given RoomService
// and (optionally) SummaryService. A nil summary service makes the
// /rooms/{id}/summary endpoint return 503 so deployments that want
// the rest of the KChat surface without AI can still boot.
func NewHandler(rooms *kchatpkg.RoomService, summary *ai.SummaryService) *Handler {
	return &Handler{rooms: rooms, summary: summary}
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
	r.Post("/rooms/{id}/summary", h.RoomSummary)
	r.Post("/attachments/upload-url", h.AttachmentUploadURL)
	r.Post("/attachments/confirm", h.AttachmentConfirm)
}

// createRoomRequest is the JSON body accepted by POST /rooms.
type createRoomRequest struct {
	KChatRoomID string `json:"kchat_room_id"`
}

// CreateRoom maps a KChat room id to a freshly-provisioned folder.
// Responds 409 when the room is already mapped so a retried POST
// surfaces the duplicate rather than silently no-op'ing. Admin-only
// because creating a room provisions a folder + grants the caller
// admin on it.
func (h *Handler) CreateRoom(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	if requireAdmin(w, r) {
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	room, err := h.rooms.CreateRoomFolder(r.Context(), workspaceID, req.KChatRoomID, userID)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, room)
}

// ListRooms returns every mapping in the workspace.
func (h *Handler) ListRooms(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	rooms, err := h.rooms.List(r.Context(), workspaceID)
	if err != nil {
		writeServiceError(w, r, err)
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
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	room, err := h.rooms.Get(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// DeleteRoom removes the mapping row. The backing folder is left
// intact so operators can keep the uploaded files; deleting the
// folder is a separate action through the regular folder API.
// Admin-only because unmapping disrupts the integration for the
// whole workspace.
func (h *Handler) DeleteRoom(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	if requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	if err := h.rooms.Delete(r.Context(), workspaceID, id); err != nil {
		writeServiceError(w, r, err)
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
// ones, and updates roles where they differ. Admin-only because
// the caller dictates the full grant set — anything less would let a
// regular member self-promote to admin on any KChat-mapped folder.
func (h *Handler) SyncMembers(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	if requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	mapping, err := h.rooms.Get(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	var req syncMembersRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	members := make([]kchatpkg.MemberSync, 0, len(req.Members))
	for _, m := range req.Members {
		uid, err := uuid.Parse(m.UserID)
		if err != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid user_id: "+m.UserID)
			return
		}
		members = append(members, kchatpkg.MemberSync{UserID: uid, Role: m.Role})
	}
	if err := h.rooms.SyncMembers(r.Context(), workspaceID, mapping.KChatRoomID, members); err != nil {
		writeServiceError(w, r, err)
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
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req attachmentUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	res, err := h.rooms.AttachmentUploadURL(r.Context(), workspaceID, req.KChatRoomID, req.Filename, req.MimeType, req.SizeBytes, userID)
	if err != nil {
		writeServiceError(w, r, err)
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
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req attachmentConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	fileID, err := uuid.Parse(req.FileID)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid file_id")
		return
	}
	res, err := h.rooms.ConfirmAttachment(r.Context(), workspaceID, fileID, req.ObjectKey, req.Checksum, req.SizeBytes, userID)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// RoomSummary returns a rule-based scaffold summary of the files in
// the folder mapped to the room. Strict-ZK folders return 403 —
// the server has no plaintext and refuses to hallucinate one.
func (h *Handler) RoomSummary(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	if h.summary == nil {
		middleware.RespondError(w, http.StatusServiceUnavailable, middleware.ErrCodeMaintenance, "summary service unavailable")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	mapping, err := h.rooms.Get(r.Context(), workspaceID, id)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	summary, err := h.summary.Summarize(r.Context(), workspaceID, mapping.FolderID)
	if err != nil {
		switch {
		case errors.Is(err, ai.ErrStrictZKForbidden):
			// Sentinel error string is stable and known
			// (defined in api/ai); exposing it in 403 is the
			// rest-of-codebase convention for sentinels.
			middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, err.Error())
		case errors.Is(err, ai.ErrFolderNotFound):
			middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, err.Error())
		default:
			// Default branch: arbitrary unrecognised err from
			// the summarisation pipeline (LLM provider 500s,
			// timeouts, panics). Route through
			// RespondInternalError so err is logged with op
			// + path + method but never reaches the JSON
			// response body. Matches the redaction contract
			// applied to writeServiceError above.
			middleware.RespondInternalError(w, r, "summarize room", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"summary": summary})
}

// writeJSON delegates to middleware.WriteJSON so kchat success
// responses share the same Content-Type charset and
// X-Content-Type-Options defence as the error responses written
// through middleware.RespondError from the same handlers. The
// prior local implementation had a nil-payload early return; no
// call site in this package passes nil (verified via grep), so
// dropping the branch is behaviour-preserving.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	middleware.WriteJSON(w, status, payload)
}

// writeServiceError translates kchat / underlying service errors
// into HTTP status codes. The default branch (unrecognised error)
// routes through middleware.RespondInternalError so the underlying
// err string is logged server-side but never appears in the JSON
// response body — same redaction contract as api/drive/helpers.go.
// Devin Review BUG_0002 on commit a2e52fb flagged the prior
// "drop err.Error() into the JSON message field" pattern as the
// codebase's biggest 500 leak vector; fix is to thread *http.Request
// through the helper so the slog logger can be reached for the
// server-side log line.
func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, kchatpkg.ErrRoomNotFound),
		errors.Is(err, file.ErrNotFound):
		middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, err.Error())
	case errors.Is(err, kchatpkg.ErrRoomAlreadyMapped):
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, err.Error())
	case errors.Is(err, kchatpkg.ErrInvalidRoomID),
		errors.Is(err, kchatpkg.ErrInvalidRole),
		errors.Is(err, kchatpkg.ErrInvalidObjectKey),
		errors.Is(err, kchatpkg.ErrInvalidSize),
		errors.Is(err, kchatpkg.ErrObjectKeyMismatch):
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
	default:
		middleware.RespondInternalError(w, r, "kchat service error", err)
	}
}
