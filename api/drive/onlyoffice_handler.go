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

// defaultOnlyOfficeMaxDocumentBytes caps how many bytes the save
// callback will buffer in memory from the Document Server's
// edited-document URL, so a hostile or runaway response cannot exhaust
// the API container (which deploy/docker-compose.prod.yml limits to
// 512 MiB). 100 MiB matches the repo's other bounded-in-memory job cap
// (index.MaxDownloadBytes) and is far above any real office document.
// It is the fallback when the handler is not wired with explicit limits
// (e.g. metadata-only test wiring); production overrides it from
// ONLYOFFICE_MAX_DOCUMENT_MB via WithOnlyOfficeSaveLimits.
const defaultOnlyOfficeMaxDocumentBytes = 100 << 20 // 100 MiB

// defaultOnlyOfficeSaveMemoryBudget is the share of the API container's
// memory the save path may buffer concurrently — half of the 512 MiB
// production container, leaving headroom for the Go runtime, caches,
// and the rest of the request load. The save path must never claim the
// whole container, or a burst of large saves would OOM everything else.
// Overridden from ONLYOFFICE_SAVE_MEMORY_BUDGET_MB in production.
const defaultOnlyOfficeSaveMemoryBudget = 256 << 20 // 256 MiB

// defaultOnlyOfficeMaxConcurrentSaves bounds how many save callbacks may
// buffer an edited document in memory at once when explicit limits are
// not wired. The editor-callback route runs outside the per-user/
// -workspace rate limiter (the Document Server holds no ZK Drive JWT),
// so without this a burst of callbacks could each buffer up to the
// per-document cap and exhaust the container. The cap is DERIVED from
// the memory budget so the worst case (concurrency * max-document)
// stays within budget by construction; excess callbacks are shed with a
// retryable error (the bytes stay in the Document Server cache and it
// retries) rather than blocking a goroutine.
const defaultOnlyOfficeMaxConcurrentSaves = defaultOnlyOfficeSaveMemoryBudget / defaultOnlyOfficeMaxDocumentBytes // = 2

// Compile-time guards on the DEFAULTS (a negative untyped constant
// converted to uint is a build error): the derived cap must be positive
// and the worst-case concurrent buffering must not exceed the budget.
// The same invariant for operator-supplied values is enforced at
// startup by config.validateOnlyOfficeGroup.
const _ = uint(defaultOnlyOfficeMaxConcurrentSaves - 1)                                                                   // cap >= 1
const _ = uint(defaultOnlyOfficeSaveMemoryBudget - defaultOnlyOfficeMaxConcurrentSaves*defaultOnlyOfficeMaxDocumentBytes) // worst case <= budget

// onlyOfficeHTTPClient pulls edited bytes from the Document Server in
// the save callback. Shared so connections are reused across callbacks.
var onlyOfficeHTTPClient = &http.Client{Timeout: onlyOfficeFetchTimeout}

// errOnlyOfficeSaveBusy is returned by the save path's bounded buffered
// fallback when its concurrency semaphore is full. It is mapped to a
// retryable 503 ack so the Document Server keeps the edited bytes and
// retries, rather than the generic 500 used for real failures. The
// primary streaming path never returns this — only the rare
// unknown-Content-Length fallback that still buffers in memory does.
var errOnlyOfficeSaveBusy = errors.New("onlyoffice: save concurrency limit reached")

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

// WithSuspensionChecker wires the workspace-suspension checker used to
// freeze callback-driven writes (the ONLYOFFICE save path) for a
// suspended workspace. The save callback runs outside the session-auth
// middleware group — the Document Server holds no ZK Drive JWT — so it
// also runs outside SuspensionGuard; this lets saveEditedVersion
// re-check suspension at the write boundary. A nil checker disables the
// check (e.g. metadata-only test wiring with no control plane), exactly
// as SuspensionGuard is a no-op when its checker is nil.
//
// The isTypedNil guard mirrors WithWebhooks / WithTagSuggester: a
// caller passing a typed-nil concrete pointer (e.g.
// (*platform.Service)(nil)) wrapped in the interface would otherwise
// compare != nil and then NPE inside ensureNotSuspended's
// WorkspaceSuspension call. Collapsing it to a real nil keeps the
// no-op-when-unwired contract intact under future conditional wiring.
func (h *Handler) WithSuspensionChecker(c middleware.WorkspaceSuspensionChecker) *Handler {
	if isTypedNil(c) {
		h.suspension = nil
		return h
	}
	h.suspension = c
	return h
}

// WithSuspensionFailClosed sets the suspension-enforcement posture for
// the ONLYOFFICE save callback to match SuspensionGuard. When true, a
// suspension-lookup error at the write boundary rejects the save
// (returning a retryable error so the Document Server keeps the bytes)
// instead of allowing it. Default (false) preserves the fail-OPEN
// availability posture. Wired from config.SuspensionFailClosed.
func (h *Handler) WithSuspensionFailClosed(failClosed bool) *Handler {
	h.suspensionFailClosed = failClosed
	return h
}

// WithOnlyOfficeSaveLimits sizes the save-callback memory guards from
// operator config: maxDocumentBytes caps a single buffered document and
// the concurrency cap is DERIVED as memoryBudgetBytes / maxDocumentBytes
// so the worst case (concurrency * max-document) stays within budget by
// construction. Non-positive inputs fall back to the package defaults,
// and the derived cap is floored at 1. config.validateOnlyOfficeGroup
// already rejects a budget below one document at startup.
func (h *Handler) WithOnlyOfficeSaveLimits(memoryBudgetBytes, maxDocumentBytes int64) *Handler {
	if maxDocumentBytes <= 0 {
		maxDocumentBytes = defaultOnlyOfficeMaxDocumentBytes
	}
	if memoryBudgetBytes <= 0 {
		memoryBudgetBytes = defaultOnlyOfficeSaveMemoryBudget
	}
	concurrency := int(memoryBudgetBytes / maxDocumentBytes)
	if concurrency < 1 {
		concurrency = 1
	}
	h.onlyOfficeMaxDocumentBytes = maxDocumentBytes
	h.onlyOfficeSaveConcurrency = concurrency
	h.onlyOfficeSaveSem = make(chan struct{}, concurrency)
	return h
}

// ensureNotSuspended returns collab.ErrWorkspaceSuspended when the
// workspace has been suspended by the platform control plane. It is a
// no-op when no checker is wired (metadata-only test wiring). On a
// suspension-lookup error it mirrors the SuspensionGuard middleware
// posture: by default it fails OPEN (suspension is an availability
// control, not a security boundary, so a transient DB blip must not
// silently drop a user's edited bytes), but when WithSuspensionFailClosed
// is set it fails CLOSED — returning a retryable error so the Document
// Server keeps the bytes and retries once the lookup recovers. Used by
// the ONLYOFFICE save callback, which runs outside the SuspensionGuard
// middleware group.
func (h *Handler) ensureNotSuspended(ctx context.Context, workspaceID uuid.UUID) error {
	if h.suspension == nil {
		return nil
	}
	suspended, _, err := h.suspension.WorkspaceSuspension(ctx, workspaceID)
	if err != nil {
		if h.suspensionFailClosed {
			logging.FromContext(ctx).Warn("onlyoffice save: suspension lookup failed; rejecting write (fail-closed)",
				"workspace_id", workspaceID, "err", err)
			return collab.ErrWorkspaceSuspended
		}
		logging.FromContext(ctx).Warn("onlyoffice save: suspension lookup failed; allowing write",
			"workspace_id", workspaceID, "err", err)
		return nil
	}
	if suspended {
		return collab.ErrWorkspaceSuspended
	}
	return nil
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

	// Authenticate the callback when a secret is configured. The token
	// (Authorization: Bearer <jwt> header, else body.token) is the
	// Document Server's JWT over the ENTIRE callback payload. On
	// success we rebuild the callback solely from the verified claims
	// and discard the unsigned body, so a spoofed body cannot steer
	// which URL we fetch or whom the save is attributed to. When no
	// secret is set the deployment is in the explicit insecure
	// local-dev mode (config.Load refuses ONLYOFFICE_URL without
	// ONLYOFFICE_SECRET unless ONLYOFFICE_ALLOW_INSECURE is set), so
	// the unsigned body is used as-is.
	if secret := h.onlyOffice.JWTSecret(); secret != "" {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			token = cb.Token
		}
		payload, vErr := h.onlyOffice.VerifyCallback(token)
		if vErr != nil || payload == nil {
			logging.FromContext(r.Context()).Warn("onlyoffice callback token verification failed", "file_id", fileID, "err", vErr)
			writeOnlyOfficeAck(w, http.StatusForbidden, 1)
			return
		}
		cb = onlyOfficeCallback{
			Status: payload.Status,
			URL:    payload.URL,
			Key:    payload.Key,
			Users:  payload.Users,
		}
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

	// The save path streams the edited document straight to object
	// storage (see saveEditedVersion / writeEditedObject), so it no
	// longer buffers whole documents in memory and needs no up-front
	// concurrency gate. Memory bounding is now applied only on the rare
	// unknown-Content-Length fallback, which shed-loads with
	// errOnlyOfficeSaveBusy → a retryable 503.
	if err := h.saveEditedVersion(ctx, workspaceID, fileID, cb); err != nil {
		if errors.Is(err, errOnlyOfficeSaveBusy) {
			logging.FromContext(ctx).Warn("onlyoffice save concurrency limit reached; asking document server to retry",
				"file_id", fileID, "limit", h.onlyOfficeSaveConcurrency)
			writeOnlyOfficeAck(w, http.StatusServiceUnavailable, 1)
			return
		}
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
	// Defense-in-depth at the write boundary: re-verify the file's
	// folder is not strict-ZK before writing plaintext bytes back.
	// GenerateEditorConfig already refuses strict-ZK at editor-open, so
	// a strict-ZK file can never legitimately reach a save callback;
	// rechecking here ensures a crafted/replayed callback can never
	// cause the server to write plaintext into a zero-knowledge folder.
	encMode, err := folder.EncryptionModeForFile(ctx, h.pool, fileID)
	if err != nil {
		return err
	}
	if encMode == folder.EncryptionStrictZK {
		return collab.ErrStrictZKForbidden
	}
	// Suspension re-check at the write boundary. The callback runs
	// outside SuspensionGuard (the Document Server has no JWT), so a
	// suspended workspace would otherwise still accept callback-driven
	// version writes while every REST write returns 503. Mirror that
	// 503 here by refusing the save.
	if err := h.ensureNotSuspended(ctx, workspaceID); err != nil {
		return err
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

	// SSRF guard at the write boundary: the edited bytes are served
	// from the Document Server's own cache, so the callback url must
	// share the configured Document Server origin. This matters most in
	// the insecure no-secret mode, where cb.URL arrives unsigned.
	if err := h.onlyOffice.ValidateDocumentURL(cb.URL); err != nil {
		return err
	}

	versionID := uuid.New()
	objectKey := storage.NewObjectKey(workspaceID, f.ID, versionID)
	contentType := "application/octet-stream"
	if f.MimeType != "" {
		contentType = f.MimeType
	}

	// Relay the edited bytes from the Document Server straight to object
	// storage. The storage quota is enforced inside writeEditedObject —
	// before any bytes are committed — exactly as the other write paths
	// (ConfirmUpload, BulkCopy) do, so an over-quota save acks non-zero
	// and the Document Server retains the bytes for retry.
	size, err := h.writeEditedObject(ctx, store, workspaceID, objectKey, contentType, cb.URL)
	if err != nil {
		return err
	}

	v := &file.FileVersion{
		ID:        versionID,
		FileID:    f.ID,
		ObjectKey: objectKey,
		SizeBytes: size,
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
		h.publishPostUploadJobs(ctx, f.ID, v.ID, f.MimeType)
		h.publishWebhookFileEvent(ctx, webhooks.EventFileUploadConfirmed, workspaceID, f, v.ID, v.SizeBytes)
	}
	return nil
}

// writeEditedObject GETs the edited document from the Document Server's
// cache URL and relays it to object storage under objectKey, returning
// the exact stored size. It enforces the per-document size cap and the
// workspace storage quota before any bytes are committed.
//
// Primary path (Content-Length known): the response body is streamed
// directly into a presigned PUT via store.PutObjectStream, so only a
// small fixed copy buffer is ever in memory regardless of document
// size — this is what lets concurrent saves run without a memory
// budget. The reader is capped at the advertised length so a body that
// over-reports cannot stream unbounded bytes.
//
// Fallback path (Content-Length absent, e.g. a chunked response the
// Document Server's cache download does not normally use): the document
// is buffered up to the per-document cap and written with PutObject.
// This path is gated by the save-concurrency semaphore so worst-case
// memory (concurrency × max-document) stays within budget; when the
// semaphore is full it returns errOnlyOfficeSaveBusy for a retryable
// 503.
func (h *Handler) writeEditedObject(ctx context.Context, store *storage.Client, workspaceID uuid.UUID, objectKey, contentType, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := onlyOfficeHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, errors.New("onlyoffice: unexpected status fetching edited document: " + resp.Status)
	}

	maxBytes := h.onlyOfficeMaxDocumentBytes

	// Primary streaming path: length is known up front.
	if resp.ContentLength > 0 {
		if resp.ContentLength > maxBytes {
			return 0, errors.New("onlyoffice: edited document exceeds the maximum allowed size")
		}
		size := resp.ContentLength
		if err := h.billing.CheckStorageQuota(ctx, workspaceID, size); err != nil {
			return 0, err
		}
		// Cap the relayed reader at the advertised length so a body
		// that lies (sends more than Content-Length) cannot stream
		// unbounded bytes; PutObjectStream sends exactly size.
		if err := store.PutObjectStream(ctx, objectKey, contentType, io.LimitReader(resp.Body, size), size); err != nil {
			return 0, err
		}
		return size, nil
	}

	// Fallback: unknown length. Bound concurrent buffering with the save
	// semaphore (shed load when full) and buffer up to the per-document
	// cap.
	if h.onlyOfficeSaveSem != nil {
		select {
		case h.onlyOfficeSaveSem <- struct{}{}:
			defer func() { <-h.onlyOfficeSaveSem }()
		default:
			return 0, errOnlyOfficeSaveBusy
		}
	}
	data, err := readLimited(resp.Body, maxBytes)
	if err != nil {
		return 0, err
	}
	if err := h.billing.CheckStorageQuota(ctx, workspaceID, int64(len(data))); err != nil {
		return 0, err
	}
	if err := store.PutObject(ctx, objectKey, contentType, data); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// readLimited reads up to max bytes from r and errors (rather than
// silently truncating) when the source is larger. It reads max+1 bytes
// so an over-read is detectable: truncating at exactly max would persist
// a corrupt, partial document version with no error surfaced to the
// Document Server or the user, so we fail the save instead and let the
// Document Server retain the bytes for retry.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("onlyoffice: edited document exceeds the maximum allowed size")
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
