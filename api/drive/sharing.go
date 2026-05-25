package drive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/email"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// Sharing DTOs -------------------------------------------------------------

type createShareLinkRequest struct {
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	Password     string  `json:"password,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	MaxDownloads *int    `json:"max_downloads,omitempty"`
}

type resolveShareLinkRequest struct {
	Password string `json:"password,omitempty"`
}

type createGuestInviteRequest struct {
	Email     string  `json:"email"`
	FolderID  string  `json:"folder_id"`
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// CreateShareLink creates a share link on a folder or file owned by the
// authenticated workspace. Admin-only per ARCHITECTURE.md §7.3 —
// sharing configuration is a sensitive operation.
func (h *Handler) CreateShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "sharing not configured")
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeAdminOnly, "admin role required")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createShareLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if !sharing.IsValidResourceType(req.ResourceType) {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid resource_type")
		return
	}
	resourceID, err := uuid.Parse(req.ResourceID)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid resource_id")
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, req.ResourceType, resourceID); err != nil {
		writeServiceError(w, err)
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid expires_at (expected RFC3339)")
			return
		}
		expiresAt = &t
	}
	link, err := h.sharing.CreateShareLink(r.Context(), workspaceID, req.ResourceType, resourceID, req.Password, expiresAt, req.MaxDownloads, userID)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	h.notify(r.Context(), func(n *notification.Service) error {
		return n.NotifyShareLinkCreated(r.Context(), workspaceID, userID, link.ID, req.ResourceType, resourceID)
	})
	writeJSON(w, http.StatusCreated, link)
}

// ResolveShareLink is the public (no-auth) endpoint used by anyone who
// holds a share-link token. When the link is password-protected the
// caller should supply the password in the JSON body of a POST; GET
// without a body is accepted for unprotected links so shares work from
// a plain browser address bar.
func (h *Handler) ResolveShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "sharing not configured")
		return
	}
	token := chi.URLParam(r, "token")
	if token == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "token is required")
		return
	}
	var password string
	if r.Method == http.MethodPost && r.ContentLength > 0 {
		var req resolveShareLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
			return
		}
		password = req.Password
	}
	link, err := h.sharing.ResolveShareLink(r.Context(), token, password)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, link)
}

// RevokeShareLink deletes a share link. Admin-only.
func (h *Handler) RevokeShareLink(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "sharing not configured")
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeAdminOnly, "admin role required")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	if err := h.sharing.RevokeShareLink(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateGuestInvite creates a guest invite and the matching permission
// grant on the target folder. Admin-only.
func (h *Handler) CreateGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "sharing not configured")
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeAdminOnly, "admin role required")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	var req createGuestInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	folderID, err := uuid.Parse(req.FolderID)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid folder_id")
		return
	}
	if err := h.assertResourceInWorkspace(r.Context(), workspaceID, permission.ResourceFolder, folderID); err != nil {
		writeServiceError(w, err)
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, terr := time.Parse(time.RFC3339, *req.ExpiresAt)
		if terr != nil {
			middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid expires_at (expected RFC3339)")
			return
		}
		expiresAt = &t
	}
	inv, err := h.sharing.CreateGuestInvite(r.Context(), workspaceID, req.Email, folderID, req.Role, expiresAt, userID)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	// Invitee notifications run on two parallel paths:
	//
	//   1. In-app notification — fires only when the invitee already
	//      has a user row in this workspace. The notification.Service
	//      schema requires a user_id, so external invitees skip this
	//      branch by design.
	//   2. Transactional email — fires for every invitee whose address
	//      is well-formed, closing the historical "external invitees
	//      receive an email out-of-band (out of scope)" gap. Failures
	//      are best-effort: a relay outage MUST NOT roll back the
	//      invite row, because the operator can re-notify out-of-band
	//      and the invite is already redeemable via its ID.
	if h.users != nil && h.notifications != nil {
		if u, uerr := h.users.GetByEmail(r.Context(), workspaceID, req.Email); uerr == nil && u != nil {
			h.notify(r.Context(), func(n *notification.Service) error {
				return n.NotifyGuestInviteSent(r.Context(), workspaceID, u.ID, inv.ID, folderID, req.Email)
			})
		}
	}
	h.dispatchGuestInviteEmail(r, inv, userID)
	writeJSON(w, http.StatusCreated, inv)
}

// guestInviteEmailDispatchTimeout caps the wall-clock budget for
// the entire detached SMTP send (dial + EHLO + optional STARTTLS +
// optional AUTH + MAIL FROM + RCPT TO + DATA + body write + QUIT).
// It feeds the context the goroutine passes to SMTPClient.Send;
// the SMTPClient threads ctx.Deadline() onto the TCP connection
// via conn.SetDeadline so post-dial reads and writes ALSO honour
// the bound (the net/smtp package's higher-level commands don't
// accept a context themselves, so without the conn-deadline plumbing
// a relay that accepts the connection then hangs mid-conversation
// would leak this goroutine forever — see the SetDeadline comment
// in internal/email/smtp.go).
const guestInviteEmailDispatchTimeout = 60 * time.Second

// dispatchGuestInviteEmail composes and sends the guest-invite
// email on a detached goroutine so a slow SMTP relay cannot
// extend the HTTP response time of CreateGuestInvite (which would
// otherwise inherit the SMTP timeout budget, ~30s, on top of the
// in-app notification path). Errors are logged and never returned
// to the caller — the invite row already exists, so a failed email
// should not change the HTTP response.
//
// The detached context is built with context.WithoutCancel so
// middleware-attached values (logger slot, workspace_id,
// user_id, trace_id) survive the response write, then wrapped
// with a hard timeout cap. The request clone (r.Clone) is the
// idiomatic way to safely use r in a goroutine after the handler
// returns: it deep-copies headers the audit service reads (User-
// Agent, RemoteAddr) and rebinds the context.
func (h *Handler) dispatchGuestInviteEmail(r *http.Request, inv *sharing.GuestInvite, inviterID uuid.UUID) {
	if h.email == nil {
		return
	}
	// Detach the request-scoped logger SLOT before enriching so
	// the goroutine reads its own logger value (not the parent's
	// shared slot). Three things would otherwise go wrong:
	//   1. logging.Enrich mutates the slot — that races with the
	//      AccessLog frame still reading the slot at response
	//      complete, AND it leaks invite_id into the access-log
	//      line of the inbound HTTP request, which is the wrong
	//      frame.
	//   2. logging.WithContext stores at loggerCtxKey, but
	//      FromContext gives slotCtxKey precedence — so the
	//      WithContext-set logger is silently shadowed by the
	//      slot's unenriched logger.
	// DetachForBackground closes the gap: it captures the slot's
	// current snapshot (workspace_id / user_id / request_id),
	// shadows the slot with a typed-nil sentinel so FromContext
	// falls through to loggerCtxKey, and re-attaches the snapshot
	// there. Subsequent .With("invite_id", ...) on the returned
	// logger lands cleanly without touching the request's slot.
	detached := context.WithoutCancel(r.Context())
	detached = logging.DetachForBackground(detached)
	detached = logging.WithContext(detached, logging.FromContext(detached).With("invite_id", inv.ID))
	sendCtx, cancel := context.WithTimeout(detached, guestInviteEmailDispatchTimeout)
	detachedR := r.Clone(sendCtx)
	go func() {
		defer cancel()

		inviterName := h.resolveInviterDisplayName(sendCtx, inv.WorkspaceID, inviterID)
		workspaceName := h.sharing.ResolveWorkspaceName(sendCtx, inv.WorkspaceID)
		folderName := h.sharing.ResolveFolderName(sendCtx, inv.WorkspaceID, inv.FolderID)

		outcome, _ := h.email.SendGuestInvite(sendCtx, email.SendGuestInviteInput{
			Email:         inv.Email,
			InviterName:   inviterName,
			WorkspaceName: workspaceName,
			FolderName:    folderName,
			Role:          inv.Role,
			InviteID:      inv.ID.String(),
			ExpiresAt:     inv.ExpiresAt,
		})
		// Service.SendGuestInvite has already logged the outcome
		// via logSendOutcome — the audit row is the second
		// observable artefact, capturing the same outcome plus
		// the actor/workspace/IP fields the audit log already
		// owns.
		h.logAudit(sendCtx, detachedR, audit.ActionGuestInviteEmailed, "guest_invite", &inv.ID, map[string]any{
			"outcome": string(outcome),
		})
	}()
}

// resolveInviterDisplayName looks up the inviter's display name
// for use in email templates. Kept handler-side (not on sharing.
// Service) because users / authentication concerns are not the
// sharing service's domain — sharing only owns workspace + folder
// resolvers. The fallback string mirrors what we use for unknown
// inviters in the audit-log surface.
func (h *Handler) resolveInviterDisplayName(ctx context.Context, workspaceID, userID uuid.UUID) string {
	if h.users == nil || userID == uuid.Nil {
		return "A workspace member"
	}
	u, err := h.users.GetByID(ctx, workspaceID, userID)
	if err != nil || u == nil {
		return "A workspace member"
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// PreviewGuestInvite returns display-safe metadata for a guest
// invite by ID. Public (unauthenticated) endpoint — the recipient
// follows the link in their email and needs workspace / folder /
// inviter context before they decide whether to sign up or log in.
// Same posture as ResolveShareLink: UUIDv4 IDs are unguessable, the
// response excludes secrets, and RLS bypasses for the unauth path.
func (h *Handler) PreviewGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "sharing not configured")
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	preview, err := h.sharing.GetGuestInvitePreview(r.Context(), id)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// AcceptGuestInvite marks an invite accepted. Authenticated endpoint —
// the caller must already hold a token bound to the invite's workspace
// (in practice the token is issued by the invite-acceptance flow).
func (h *Handler) AcceptGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "sharing not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	inv, err := h.sharing.AcceptGuestInvite(r.Context(), workspaceID, id)
	if err != nil {
		writeSharingError(w, err)
		return
	}
	h.notify(r.Context(), func(n *notification.Service) error {
		return n.NotifyGuestInviteAccepted(r.Context(), workspaceID, inv.CreatedBy, inv.ID, inv.Email)
	})
	writeJSON(w, http.StatusOK, inv)
}

// RevokeGuestInvite deletes an invite. Admin-only.
func (h *Handler) RevokeGuestInvite(w http.ResponseWriter, r *http.Request) {
	if h.sharing == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "sharing not configured")
		return
	}
	role, _ := middleware.RoleFromContext(r.Context())
	if role != user.RoleAdmin {
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeAdminOnly, "admin role required")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid id")
		return
	}
	if err := h.sharing.RevokeGuestInvite(r.Context(), workspaceID, id); err != nil {
		writeSharingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSharingError maps sharing-service errors to HTTP responses.
// ErrNotFound becomes 404, invalid-input errors become 400, expired /
// exhausted / password-related errors become 410 / 429 / 401 / 403 so
// clients can distinguish them without parsing the error text.
func writeSharingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sharing.ErrNotFound):
		middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, err.Error())
	case errors.Is(err, sharing.ErrInvalidResourceType),
		errors.Is(err, sharing.ErrInvalidRole),
		errors.Is(err, sharing.ErrInvalidEmail):
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, err.Error())
	case errors.Is(err, sharing.ErrLinkExpired),
		errors.Is(err, sharing.ErrInviteExpired):
		middleware.RespondError(w, http.StatusGone, middleware.ErrCodeGone, err.Error())
	case errors.Is(err, sharing.ErrLinkExhausted):
		middleware.RespondError(w, http.StatusTooManyRequests, middleware.ErrCodeRateLimit, err.Error())
	case errors.Is(err, sharing.ErrPasswordRequired):
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, err.Error())
	case errors.Is(err, sharing.ErrPasswordIncorrect):
		middleware.RespondError(w, http.StatusForbidden, middleware.ErrCodeForbidden, err.Error())
	case errors.Is(err, sharing.ErrInviteAlreadyUsed):
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, err.Error())
	default:
		writeServiceError(w, err)
	}
}
