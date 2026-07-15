package drive

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// presignCacheSafetyMargin is subtracted from a presigned URL's
// validity window when deriving the Cache-Control max-age for the JSON
// response that carries the URL. A client (or its forward/browser
// cache) that re-uses a cached download-URL response must never hand
// back a URL that has already expired at the storage gateway, so we
// stop advertising the response as fresh a full minute before the
// underlying URL lapses. This absorbs clock skew between the API pod
// and the client plus the round-trip the client still needs to make to
// storage after reading the cached response.
const presignCacheSafetyMargin = 60 * time.Second

// setPresignedURLCacheControl marks a response that carries a
// presigned URL as privately cacheable for slightly less than the
// URL's own validity window.
//
//   - `private`: a presigned URL is a bearer capability scoped to one
//     authenticated user. It MUST NOT land in a shared/CDN cache keyed
//     only on the request path, or one tenant's capability would be
//     served to another. `private` confines reuse to the end user's
//     own browser cache, which is exactly the repeat-click / resumed
//     download case we want to accelerate.
//   - `max-age`: the URL's remaining lifetime minus
//     presignCacheSafetyMargin, floored at 0. A non-positive budget
//     emits `no-store` so a URL too short-lived to safely cache is
//     never advertised as fresh.
//
// Callers pass the same ttl they handed to the storage presigner so
// the HTTP freshness window and the URL's cryptographic expiry stay in
// lockstep.
func setPresignedURLCacheControl(w http.ResponseWriter, ttl time.Duration) {
	maxAge := ttl - presignCacheSafetyMargin
	if maxAge <= 0 {
		w.Header().Set("Cache-Control", "no-store")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(maxAge.Seconds())))
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

// writeServiceError maps a service-layer error to the canonical
// JSON envelope. The classification matches the (mostly internal)
// sentinel errors raised by the file / folder / workspace / user /
// permission packages, plus the badRequestErr / forbiddenErr
// dynamic-type carve-outs used inside drive handlers.
//
// The default branch — unrecognised error type — is the codebase's
// biggest err.Error() leak vector: every database
// failure, context-timeout, or upstream panic that bubbled up to a
// drive handler landed in a 500 response with the raw Go error
// string in the JSON `message` field. The helper now takes
// *http.Request so the default branch can route through
// middleware.RespondInternalError, which logs the underlying err
// to the request-scoped slog logger (operators get the diagnostic)
// and writes a sanitised 500 envelope (clients see just
// "drive service error"). The *http.Request had to be threaded
// through the helper because writeServiceError previously had no
// access to it, so routing the default branch through
// middleware.RespondInternalError was not a one-line replacement.
func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, folder.ErrNotFound),
		errors.Is(err, file.ErrNotFound),
		errors.Is(err, workspace.ErrNotFound),
		errors.Is(err, user.ErrNotFound),
		errors.Is(err, permission.ErrNotFound):
		middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, err.Error())
	case errors.Is(err, folder.ErrInvalidName),
		errors.Is(err, folder.ErrInvalidParent),
		errors.Is(err, folder.ErrInvalidEncryptionMode),
		errors.Is(err, file.ErrInvalidName),
		errors.Is(err, notification.ErrInvalidSubscription),
		errors.Is(err, notification.ErrInvalidDeviceToken):
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeValidation, err.Error())
	case errors.Is(err, notification.ErrPlatformUnsupported):
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, err.Error())
	case errors.Is(err, folder.ErrEncryptionModeMismatch):
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, err.Error())
	case errors.Is(err, file.ErrDuplicateName):
		middleware.RespondError(w, http.StatusConflict, "FILE_NAME_EXISTS", "a file with this name already exists in this folder")
	case errors.Is(err, file.ErrVersionConflict):
		// Surface the generic 409 rather than err.Error() so the
		// internal "file version conflicts with existing row" detail
		// (which encodes the storage-layer invariant violation) does
		// not leak to the client. The only way to trip this branch
		// is a UUID forge attempt or a programming error, so a
		// terse message is appropriate.
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, "version conflict")
	default:
		var br badRequestErr
		var fb forbiddenErr
		if errors.As(err, &br) {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
			return
		}
		if errors.As(err, &fb) {
			middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, err.Error())
			return
		}
		middleware.RespondInternalError(w, r, "drive service error", err)
	}
}

// writeJSON delegates to middleware.WriteJSON so success responses
// share the same Content-Type charset and X-Content-Type-Options
// defence as error responses written through middleware.RespondError.
// Kept as a thin package-local alias so the rest of the package
// reads the same as before (writeJSON(w, status, payload)) rather
// than every call site importing middleware.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	middleware.WriteJSON(w, status, payload)
}
