package drive

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// parseIntParam parses a query-string int with a default. Negative values
// fall back to def so a malicious "?limit=-1" can't break the SQL.
func parseIntParam(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func (h *Handler) requireWorkspaceMatch(r *http.Request) (*workspace.Workspace, error) {
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	paramID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return nil, badRequestErr{"invalid id"}
	}
	if paramID != workspaceID {
		return nil, forbiddenErr{"workspace mismatch"}
	}
	ws, err := h.workspaces.GetByID(r.Context(), workspaceID)
	if err != nil {
		return nil, err
	}
	return ws, nil
}

type badRequestErr struct{ msg string }

func (e badRequestErr) Error() string { return e.msg }

type forbiddenErr struct{ msg string }

func (e forbiddenErr) Error() string { return e.msg }

// sameFolderEncryptionMode treats an empty string as the default
// managed-encrypted mode so rows from before migration 018 are still
// considered managed-encrypted on the file-move path.
func sameFolderEncryptionMode(a, b string) bool {
	if a == "" {
		a = folder.EncryptionManagedEncrypted
	}
	if b == "" {
		b = folder.EncryptionManagedEncrypted
	}
	return a == b
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, folder.ErrNotFound),
		errors.Is(err, file.ErrNotFound),
		errors.Is(err, workspace.ErrNotFound),
		errors.Is(err, user.ErrNotFound),
		errors.Is(err, permission.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, folder.ErrInvalidName),
		errors.Is(err, folder.ErrInvalidParent),
		errors.Is(err, folder.ErrInvalidEncryptionMode),
		errors.Is(err, file.ErrInvalidName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, folder.ErrEncryptionModeMismatch):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, file.ErrVersionConflict):
		// Surface the generic 409 rather than err.Error() so the
		// internal "file version conflicts with existing row" detail
		// (which encodes the storage-layer invariant violation) does
		// not leak to the client. The only way to trip this branch
		// is a UUID forge attempt or a programming error, so a
		// terse message is appropriate.
		http.Error(w, "version conflict", http.StatusConflict)
	default:
		var br badRequestErr
		var fb forbiddenErr
		if errors.As(err, &br) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if errors.As(err, &fb) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
