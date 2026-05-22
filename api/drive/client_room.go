package drive

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// Client room DTOs ---------------------------------------------------------

type createClientRoomRequest struct {
	Name           string  `json:"name"`
	Password       string  `json:"password,omitempty"`
	ExpiresAt      *string `json:"expires_at,omitempty"`
	DropboxEnabled bool    `json:"dropbox_enabled,omitempty"`
}

// clientRoomResponse envelopes the room row alongside the share-link
// token so the client has everything it needs to display the public
// URL after a single round-trip. We don't inline the entire ShareLink
// struct — only the token and link id — to keep the client API narrow.
type clientRoomResponse struct {
	*sharing.ClientRoom
	ShareLinkToken string `json:"share_link_token"`
}

// createClientRoomFromTemplateRequest is the JSON body for
// POST /api/client-rooms/from-template. Same fields as the regular
// create payload plus the template name.
type createClientRoomFromTemplateRequest struct {
	createClientRoomRequest
	Template string `json:"template"`
}

// clientRoomTemplateResponse extends clientRoomResponse with the
// list of sub-folder ids the template materialised, in template
// order. The list lets the UI immediately render the new structure
// without round-tripping back to /api/folders.
type clientRoomTemplateResponse struct {
	clientRoomResponse
	SubFolderIDs []uuid.UUID `json:"sub_folder_ids"`
}

// CreateClientRoom provisions a new client room (folder + share link
// bundle) owned by the authenticated workspace. Admin-only per the
// same reasoning as share-link / guest-invite creation: configuring
// external access is a sensitive operation.
func (h *Handler) CreateClientRoom(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createClientRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			http.Error(w, "invalid expires_at (expected RFC3339)", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}
	room, link, err := h.clientRooms.Create(r.Context(), workspaceID, userID, sharing.ClientRoomInput{
		Name:           req.Name,
		Password:       req.Password,
		ExpiresAt:      expiresAt,
		DropboxEnabled: req.DropboxEnabled,
	})
	if err != nil {
		if errors.Is(err, sharing.ErrInvalidRoomName) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, clientRoomResponse{ClientRoom: room, ShareLinkToken: link.Token})
}

// CreateClientRoomFromTemplate provisions a client room and seeds it
// with sub-folders from a built-in template (agency, accounting,
// legal, construction, clinic). Admin-only for the same reasons as
// CreateClientRoom — public-link configuration is sensitive.
func (h *Handler) CreateClientRoomFromTemplate(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createClientRoomFromTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			http.Error(w, "invalid expires_at (expected RFC3339)", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}
	room, link, subIDs, err := h.clientRooms.CreateFromTemplate(r.Context(), workspaceID, userID, sharing.ClientRoomInput{
		Name:           req.Name,
		Password:       req.Password,
		ExpiresAt:      expiresAt,
		DropboxEnabled: req.DropboxEnabled,
	}, req.Template)
	if err != nil {
		switch {
		case errors.Is(err, sharing.ErrUnknownTemplate):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, sharing.ErrInvalidRoomName):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		default:
			writeSharingError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, clientRoomTemplateResponse{
		clientRoomResponse: clientRoomResponse{ClientRoom: room, ShareLinkToken: link.Token},
		SubFolderIDs:       subIDs,
	})
}

// ListClientRoomTemplates returns every built-in template so the
// client can render a picker without hard-coding the list.
func (h *Handler) ListClientRoomTemplates(w http.ResponseWriter, r *http.Request) {
	tpls := sharing.ListTemplates()
	out := make([]map[string]any, 0, len(tpls))
	for _, t := range tpls {
		out = append(out, map[string]any{
			"name":        t.Name,
			"sub_folders": t.SubFolders,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// ListClientRooms returns every room in the workspace, newest first.
// Any authenticated workspace member may list; room contents are still
// gated by folder-level permissions downstream.
func (h *Handler) ListClientRooms(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	rooms, err := h.clientRooms.List(r.Context(), workspaceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

// GetClientRoom returns a single room. Scoped to workspace.
func (h *Handler) GetClientRoom(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	room, err := h.clientRooms.Get(r.Context(), workspaceID, id)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// DeleteClientRoom tears down a room (revoke share link + delete room
// row). Admin-only. The backing folder is intentionally not deleted;
// see ClientRoomService.Delete for rationale.
func (h *Handler) DeleteClientRoom(w http.ResponseWriter, r *http.Request) {
	if h.clientRooms == nil {
		http.Error(w, "client rooms not configured", http.StatusNotImplemented)
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.clientRooms.Delete(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
