package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
)

// sessionInfo is the client-facing view of a single active session.
// It deliberately omits the device_hash fingerprint stored server-side
// — that value is an internal anomaly-detection input, not something a
// client needs (or should be able to enumerate). Current flags the
// session the caller is making this request from so the UI can label
// "this device" and warn before revoking it.
type sessionInfo struct {
	SessionID  string    `json:"session_id"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Current    bool      `json:"current"`
}

type sessionsListResponse struct {
	Sessions []sessionInfo `json:"sessions"`
}

// ListSessions returns the caller's active device sessions, newest
// activity first, for the account-security "where you're signed in"
// surface (6.2). Scoped strictly to the authenticated user and
// workspace from the JWT, so one user can never enumerate another's
// sessions.
func (h *Handler) ListSessions(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	// No store wired (single-process dev / stateless deployments):
	// there are no tracked sessions, so report an empty list rather
	// than erroring — the surface degrades gracefully.
	if h.sessions == nil {
		writeJSON(w, http.StatusOK, sessionsListResponse{Sessions: []sessionInfo{}})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), middleware.SessionCheckTimeout)
	recs, err := h.sessions.ListForUser(ctx, claims.WorkspaceID, claims.UserID)
	cancel()
	if err != nil {
		middleware.RespondInternalError(w, r, "list sessions", err)
		return
	}

	out := make([]sessionInfo, 0, len(recs))
	for _, rec := range recs {
		out = append(out, sessionInfo{
			SessionID:  rec.SessionID,
			UserAgent:  rec.UserAgent,
			IP:         rec.IP,
			CreatedAt:  rec.CreatedAt,
			LastSeenAt: rec.LastSeenAt,
			Current:    rec.SessionID == claims.SessionID,
		})
	}
	writeJSON(w, http.StatusOK, sessionsListResponse{Sessions: out})
}

// RevokeSession revokes a single session owned by the caller (6.2):
// DELETE /api/auth/sessions/:id. The store enforces ownership, so a
// caller can only ever revoke their own device; an unknown or
// already-gone id yields 404. Revoking the current session is allowed
// and simply 401s the caller's next request via AuthMiddleware's
// session-existence check.
func (h *Handler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	if h.sessions == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "session store not wired")
		return
	}
	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "session id is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), middleware.SessionCheckTimeout)
	deleted, err := h.sessions.RevokeForUser(ctx, claims.WorkspaceID, claims.UserID, sessionID)
	cancel()
	if err != nil {
		middleware.RespondInternalError(w, r, "revoke session", err)
		return
	}
	if !deleted {
		middleware.RespondError(w, http.StatusNotFound, middleware.ErrCodeNotFound, "session not found")
		return
	}

	actor := claims.UserID
	h.logAudit(r.Context(), claims.WorkspaceID, &actor, audit.ActionSessionRevoke, r, map[string]any{
		"session_id": sessionID,
		"current":    sessionID == claims.SessionID,
	})
	w.WriteHeader(http.StatusNoContent)
}
