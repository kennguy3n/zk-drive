package drive

// onlyoffice_handler.go exposes the HTTP surface for ONLYOFFICE
// Document Server integration:
//
//   GET  /api/files/{id}/editor-config   — authenticated; returns the
//        signed DocEditor config the browser hands to DocsAPI.
//   POST /api/files/{id}/editor-callback — UNAUTHENTICATED (no session
//        JWT); the Document Server POSTs here when a document is ready
//        to save. Authenticated by the ONLYOFFICE-signed body/header
//        token + the workspace_id query param the config embedded.
//   GET  /api/onlyoffice/status          — authenticated feature flag
//        so the frontend can show/hide the "Open in Editor" affordance.
//
// The orchestration lives in internal/collab/onlyoffice.go; this file
// is the thin adapter that wires the drive services into the
// collab.EditorDataSource contract and translates service errors to
// the HTTP error taxonomy.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/activity"
	"github.com/kennguy3n/zk-drive/internal/collab"
	"github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
)

// onlyOfficeFetchTimeout bounds the GET the callback makes to pull the
// edited document back from the Document Server's cache. The server is
// typically co-located, so the window is generous but finite to avoid
// a hung callback pinning a goroutine.
const onlyOfficeFetchTimeout = 60 * time.Second

// onlyOfficeMaxDocumentBytes caps how many bytes the save callback will
// read from the Document Server's edited-document URL, so a hostile or
// runaway response cannot exhaust memory. Office documents are far
// below this bound.
const onlyOfficeMaxDocumentBytes = 512 << 20 // 512 MiB

// onlyOfficeHTTPClient pulls edited bytes from the Document Server in
// the save callback. Shared so connections are reused across callbacks.
var onlyOfficeHTTPClient = &http.Client{Timeout: onlyOfficeFetchTimeout}

// ONLYOFFICE callback status codes (Document Server → ZK Drive).
const (
	onlyOfficeStatusReadyForSaving = 2 // editing finished, save now
	onlyOfficeStatusForceSave      = 6 // periodic save while still editing
)

// WithOnlyOffice wires the ONLYOFFICE Document Server integration.
// serverURL is the Document Server base URL (empty disables the
// feature gracefully), jwtSecret is the shared callback-verification
// secret, and publicURL is ZK Drive's externally reachable base URL
// used to compose the absolute callbackUrl the Document Server POSTs
// to. The EditorDataSource is backed by this handler's own file /
// folder / permission / storage services, so no extra dependencies
// need threading through cmd/server/main.go.
func (h *Handler) WithOnlyOffice(serverURL, jwtSecret, publicURL string) *Handler {
	h.onlyOffice = collab.NewOnlyOfficeService(serverURL, jwtSecret, publicURL, &driveEditorData{h: h})
	return h
}

// driveEditorData adapts the drive Handler's services to the
// collab.EditorDataSource contract.
type driveEditorData struct {
	h *Handler
}

// FileForEditor returns the file plus its current version's object key.
func (d *driveEditorData) FileForEditor(ctx context.Context, workspaceID, fileID uuid.UUID) (collab.EditorFile, error) {
	f, err := d.h.files.GetByID(ctx, workspaceID, fileID)
	if err != nil {
		return collab.EditorFile{}, err
	}
	ef := collab.EditorFile{
		WorkspaceID: workspaceID,
		FileID:      f.ID,
		Name:        f.Name,
	}
	if f.CurrentVersionID == nil {
		return ef, nil // empty ObjectKey → service returns ErrNoCurrentVersion
	}
	v, err := d.h.files.GetVersionByID(ctx, workspaceID, *f.CurrentVersionID)
	if err != nil {
		if errors.Is(err, file.ErrNotFound) {
			return ef, nil
		}
		return collab.EditorFile{}, err
	}
	ef.ObjectKey = v.ObjectKey
	return ef, nil
}

// EncryptionMode reports the encryption mode of the folder owning the
// file. An empty string (missing row) is the managed-encrypted default.
func (d *driveEditorData) EncryptionMode(ctx context.Context, _ uuid.UUID, fileID uuid.UUID) (string, error) {
	return folder.EncryptionModeForFile(ctx, d.h.pool, fileID)
}

// CanEdit / CanView mirror assertResourceAccess's policy: admins pass
// unconditionally, a nil permission service is fail-open (matches the
// metadata-only deployment mode), otherwise the inheritance-aware
// grant check decides.
func (d *driveEditorData) CanEdit(ctx context.Context, workspaceID, fileID, userID uuid.UUID) (bool, error) {
	return d.hasRole(ctx, workspaceID, fileID, userID, permission.RoleEditor)
}

func (d *driveEditorData) CanView(ctx context.Context, workspaceID, fileID, userID uuid.UUID) (bool, error) {
	return d.hasRole(ctx, workspaceID, fileID, userID, permission.RoleViewer)
}

func (d *driveEditorData) hasRole(ctx context.Context, workspaceID, fileID, userID uuid.UUID, minRole string) (bool, error) {
	if d.h.permissions == nil {
		return true, nil
	}
	if role, _ := middleware.RoleFromContext(ctx); role == user.RoleAdmin {
		return true, nil
	}
	return d.h.permissions.HasAccessWithInheritance(ctx, workspaceID, permission.ResourceFile, fileID, permission.GranteeUser, userID, minRole)
}

// PresignedDownloadURL mints a time-limited GET URL the Document
// Server uses to pull the current bytes.
func (d *driveEditorData) PresignedDownloadURL(ctx context.Context, workspaceID uuid.UUID, objectKey string, ttl time.Duration) (string, error) {
	store := d.h.resolveStorage(ctx, workspaceID)
	if store == nil {
		return "", errors.New("drive: storage not configured")
	}
	return store.GenerateDownloadURL(ctx, objectKey, ttl)
}

// onlyOfficeStatusResponse is the GET /api/onlyoffice/status payload.
type onlyOfficeStatusResponse struct {
	Enabled bool `json:"enabled"`
}

// OnlyOfficeStatus reports whether office-document editing is
// configured so the frontend can gate the "Open in Editor" button.
func (h *Handler) OnlyOfficeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, onlyOfficeStatusResponse{Enabled: h.onlyOffice != nil && h.onlyOffice.Enabled()})
}

// EditorConfig returns the signed ONLYOFFICE DocEditor config for the
// file. The requested mode defaults to "edit" and is downgraded to
// "view" by the service when the caller lacks editor access.
func (h *Handler) EditorConfig(w http.ResponseWriter, r *http.Request) {
	if h.onlyOffice == nil || !h.onlyOffice.Enabled() {
		middleware.RespondError(w, http.StatusServiceUnavailable, middleware.ErrCodeUnsupportedOp, "office editing not configured")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())
	fileID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}

	mode := collab.ModeEdit
	if strings.EqualFold(r.URL.Query().Get("mode"), collab.ModeView) {
		mode = collab.ModeView
	}

	cfg, err := h.onlyOffice.GenerateEditorConfig(r.Context(), workspaceID, fileID, userID, h.editorUserName(r.Context(), workspaceID, userID), mode)
	if err != nil {
		writeOnlyOfficeError(w, r, err)
		return
	}
	h.logActivity(r.Context(), activity.ActionFileDownload, permission.ResourceFile, fileID, map[string]any{
		"editor": "onlyoffice",
		"mode":   cfg.EditorConfig.Mode,
	})
	writeJSON(w, http.StatusOK, cfg)
}

// editorUserName resolves a human label for the editing user for
// co-editing presence. Falls back to a short id slug if the user
// service is unwired or the lookup fails.
func (h *Handler) editorUserName(ctx context.Context, workspaceID, userID uuid.UUID) string {
	if h.users != nil {
		if u, err := h.users.GetByID(ctx, workspaceID, userID); err == nil && u != nil {
			if name := strings.TrimSpace(u.Name); name != "" {
				return name
			}
			if u.Email != "" {
				return u.Email
			}
		}
	}
	s := userID.String()
	if len(s) > 8 {
		s = s[:8]
	}
	return "User " + s
}

// onlyOfficeCallback is the inbound Document Server save payload. Only
// the fields ZK Drive acts on are modelled; the Document Server sends
// more (history, changesurl, …) which we ignore.
type onlyOfficeCallback struct {
	Key    string   `json:"key"`
	Status int      `json:"status"`
	URL    string   `json:"url"`
	Users  []string `json:"users"`
	Token  string   `json:"token"`
}

// EditorCallback handles the Document Server save callback. It is
// mounted OUTSIDE the session-auth group: the Document Server has no
// ZK Drive JWT, so the request is authenticated by the ONLYOFFICE
// body/header token (verified against ONLYOFFICE_SECRET) plus the
// workspace_id query param the editor-config embedded in the
// callbackUrl. On status 2 / 6 the edited bytes are pulled and written
// back as a new file version.
//
// The Document Server expects a JSON ack of the exact shape
// {"error": 0} on success (non-zero error makes it retry).
func (h *Handler) EditorCallback(w http.ResponseWriter, r *http.Request) {
	if h.onlyOffice == nil || !h.onlyOffice.Enabled() {
		writeOnlyOfficeAck(w, http.StatusServiceUnavailable, 1)
		return
	}
	workspaceID, err := uuid.Parse(r.URL.Query().Get("workspace_id"))
	if err != nil {
		writeOnlyOfficeAck(w, http.StatusBadRequest, 1)
		return
	}
	fileID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeOnlyOfficeAck(w, http.StatusBadRequest, 1)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOnlyOfficeAck(w, http.StatusBadRequest, 1)
		return
	}
	var cb onlyOfficeCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		writeOnlyOfficeAck(w, http.StatusBadRequest, 1)
		return
	}

	// Verify the ONLYOFFICE token when a secret is configured. The
	// token (Authorization: Bearer <jwt> header, else body.token)
	// wraps the callback fields, so on success the signed claims — not
	// the unsigned request body — are the sole source of truth for
	// status/url/users, defeating a spoofed body paired with a stolen
	// but unrelated token.
	if secret := h.onlyOffice.JWTSecret(); secret != "" {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			token = cb.Token
		}
		claims, vErr := h.onlyOffice.VerifyCallbackToken(token)
		if vErr != nil || claims == nil {
			logging.FromContext(r.Context()).Warn("onlyoffice callback token verification failed", "file_id", fileID, "err", vErr)
			writeOnlyOfficeAck(w, http.StatusForbidden, 1)
			return
		}
		status, url, users, ok := collab.CallbackClaims(claims)
		if !ok {
			logging.FromContext(r.Context()).Warn("onlyoffice callback token missing status claim", "file_id", fileID)
			writeOnlyOfficeAck(w, http.StatusForbidden, 1)
			return
		}
		// Overwrite from the verified claims unconditionally so the
		// unsigned body cannot influence what we fetch or attribute.
		cb.Status, cb.URL, cb.Users = status, url, users
	}

	// Only the "ready for saving" / "force save" statuses carry new
	// bytes. Everything else (being edited / closed-no-changes /
	// errors) is acknowledged without a write so the Document Server
	// stops retrying.
	if cb.Status != onlyOfficeStatusReadyForSaving && cb.Status != onlyOfficeStatusForceSave {
		writeOnlyOfficeAck(w, http.StatusOK, 0)
		return
	}
	if cb.URL == "" {
		writeOnlyOfficeAck(w, http.StatusBadRequest, 1)
		return
	}

	// This handler runs outside the session-auth middleware group, so
	// the request context carries no tenant scope. Bind the workspace id
	// (parsed from the signed callbackUrl) so downstream RLS-aware
	// queries, the activity log, the changefeed, and webhook actors are
	// correctly scoped. The editing user is injected inside
	// saveEditedVersion once the file's fallback creator is known.
	ctx := middleware.WithWorkspaceID(r.Context(), workspaceID)
	if err := h.saveEditedVersion(ctx, workspaceID, fileID, cb); err != nil {
		logging.FromContext(ctx).Error("onlyoffice save edited version failed", "file_id", fileID, "err", err)
		// Non-zero error tells the Document Server to retry later;
		// the edited bytes stay in its cache until then.
		writeOnlyOfficeAck(w, http.StatusInternalServerError, 1)
		return
	}
	writeOnlyOfficeAck(w, http.StatusOK, 0)
}

// saveEditedVersion pulls the edited document from the Document
// Server, writes it to zk-object-fabric under a fresh version key, and
// records the new file_version (advancing current_version_id + size).
func (h *Handler) saveEditedVersion(ctx context.Context, workspaceID, fileID uuid.UUID, cb onlyOfficeCallback) error {
	f, err := h.files.GetByID(ctx, workspaceID, fileID)
	if err != nil {
		return err
	}
	// Re-check the folder's encryption mode at save time, not just when
	// the editor config was issued. An admin can flip a folder
	// managed_encrypted -> strict_zk during an active edit session;
	// without this guard the callback would write Document-Server
	// plaintext into what is now a zero-knowledge folder, breaking the
	// strict-ZK contract. Mirrors the check in GenerateEditorConfig.
	encMode, err := folder.EncryptionModeForFile(ctx, h.pool, fileID)
	if err != nil {
		return err
	}
	if encMode == folder.EncryptionStrictZK {
		return collab.ErrStrictZKForbidden
	}
	// Attribute the save to the callback's editing user (falling back to
	// the file creator) so the activity log, changefeed, and webhook
	// actor are recorded against a real user rather than uuid.Nil.
	author := callbackAuthor(cb, f.CreatedBy)
	ctx = middleware.WithUserID(ctx, author)
	store := h.resolveStorage(ctx, workspaceID)
	if store == nil {
		return errors.New("drive: storage not configured")
	}

	data, err := fetchEditedDocument(ctx, cb.URL)
	if err != nil {
		return err
	}

	// Enforce the storage quota before committing the new version, the
	// same pre-flight every other write path runs (ConfirmUpload,
	// BulkCopy). Returning the error makes the callback ack non-zero, so
	// the Document Server keeps the edited bytes and retries rather than
	// silently letting the workspace exceed its plan.
	if err := h.billing.CheckStorageQuota(ctx, workspaceID, int64(len(data))); err != nil {
		return err
	}

	versionID := uuid.New()
	objectKey := storage.NewObjectKey(workspaceID, f.ID, versionID)
	contentType := "application/octet-stream"
	if f.MimeType != "" {
		contentType = f.MimeType
	}
	if err := store.PutObject(ctx, objectKey, contentType, data); err != nil {
		return err
	}

	v := &file.FileVersion{
		ID:        versionID,
		FileID:    f.ID,
		ObjectKey: objectKey,
		SizeBytes: int64(len(data)),
		CreatedBy: author,
	}
	fresh, err := h.files.ConfirmVersion(ctx, workspaceID, v)
	if err != nil {
		// The object is already in storage but no version row references
		// it. Unlike the direct-upload path, this server-side save sets
		// no pending_upload_object_key marker, so the orphan GC would
		// never reclaim it; best-effort delete it here to avoid leaking.
		if delErr := store.DeleteObject(ctx, objectKey); delErr != nil {
			logging.FromContext(ctx).Warn("onlyoffice: failed to delete orphaned object after confirm failure", "object_key", objectKey, "err", delErr)
		}
		return err
	}
	if fresh {
		h.logActivity(ctx, activity.ActionFileUpload, permission.ResourceFile, f.ID, map[string]any{
			"version_id": v.ID,
			"size_bytes": v.SizeBytes,
			"source":     "onlyoffice",
		})
		h.billing.RecordUpload(ctx, workspaceID, v.SizeBytes)
		// Re-run preview / scan / index on the new bytes, mirroring
		// the direct-upload confirm path.
		h.publishPostUploadJobs(ctx, f.ID, v.ID)
		h.publishWebhookFileEvent(ctx, webhooks.EventFileUploadConfirmed, workspaceID, f, v.ID, v.SizeBytes)
	}
	return nil
}

// fetchEditedDocument GETs the edited file from the Document Server's
// cache URL.
func fetchEditedDocument(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := onlyOfficeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("onlyoffice: unexpected status fetching edited document: " + resp.Status)
	}
	// Cap the read so a hostile / runaway Document Server response cannot
	// exhaust memory; office documents are far below this bound. Read one
	// byte past the cap so an over-limit response is detected and rejected
	// rather than silently truncated and stored as a corrupt version — a
	// non-zero ack keeps the bytes in the Document Server's cache.
	data, err := io.ReadAll(io.LimitReader(resp.Body, onlyOfficeMaxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > onlyOfficeMaxDocumentBytes {
		return nil, errors.New("onlyoffice: edited document exceeds maximum allowed size")
	}
	return data, nil
}

// callbackAuthor picks the user id to attribute the new version to:
// the first parseable id from the callback's users array, falling back
// to the file's original creator when the array is absent / malformed.
func callbackAuthor(cb onlyOfficeCallback, fallback uuid.UUID) uuid.UUID {
	for _, u := range cb.Users {
		if id, err := uuid.Parse(u); err == nil {
			return id
		}
	}
	return fallback
}

// bearerToken extracts the token from an "Authorization: Bearer <tok>"
// header value, returning "" when the scheme is absent.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

// writeOnlyOfficeAck writes the {"error": n} body the Document Server
// expects. n=0 means success; non-zero triggers a retry.
func writeOnlyOfficeAck(w http.ResponseWriter, httpStatus, errorCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]int{"error": errorCode})
}

// writeOnlyOfficeError maps OnlyOffice service errors to the HTTP error
// taxonomy. Anything unrecognised falls through to writeServiceError.
func writeOnlyOfficeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, collab.ErrOnlyOfficeNotConfigured):
		middleware.RespondError(w, http.StatusServiceUnavailable, middleware.ErrCodeUnsupportedOp, "office editing not configured")
	case errors.Is(err, collab.ErrStrictZKForbidden):
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, "office editing is unavailable for zero-knowledge folders")
	case errors.Is(err, collab.ErrEditorAccessDenied):
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, "insufficient permission to open editor")
	case errors.Is(err, collab.ErrNoCurrentVersion):
		middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "file has no current version to edit")
	case errors.Is(err, collab.ErrUnsupportedDocumentType):
		middleware.RespondError(w, http.StatusUnsupportedMediaType, middleware.ErrCodeUnsupportedOp, "file type not supported by the office editor")
	default:
		writeServiceError(w, r, err)
	}
}
