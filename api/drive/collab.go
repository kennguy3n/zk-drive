package drive

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/collab"
	"github.com/kennguy3n/zk-drive/internal/document"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/session"
)

// Read/write pump tunables for collab connections. Larger
// maxMessageSize than the notification ws (4 KiB) because Yjs
// updates carry binary payloads up to MaxDeltaPayloadBytes (1 MiB).
// pongWait is generous so flaky-network clients don't churn rooms.
const (
	collabWriteWait      = 10 * time.Second
	collabPongWait       = 60 * time.Second
	collabPingPeriod     = (collabPongWait * 9) / 10
	collabMaxMessageSize = collab.MaxFrameBytes
)

// collabReauthInterval bounds how often a live collab connection
// re-checks its authorization (token expiry + session revocation).
//
// A collab WebSocket is authenticated exactly once — by AuthMiddleware,
// before the upgrade — and is then long-lived (a single editing
// session can stay open for hours). Without a periodic re-check the
// socket would keep relaying edits long after its JWT expired or its
// session was revoked. 30s keeps the post-revocation exposure window
// small while adding only two bounded store reads per connection per
// interval (negligible next to the per-request checks AuthMiddleware
// already runs on the HTTP plane). Expiry is also enforced within this
// window — acceptable overrun against a multi-hour token TTL.
const collabReauthInterval = 30 * time.Second

// Collab connection close codes for server-side auth-lifetime
// enforcement (see collabAuthPump). The frontend CollabProvider treats
// a fixed set of codes as PERMANENT (it will not reconnect) and every
// other code as RETRIABLE (exponential-backoff reconnect, which
// re-runs tokenProvider() to fetch a fresh JWT). We pick codes
// deliberately on each side of that line:
//
//   - collabCloseReauthRequired (4002) is NOT in the client's
//     PermanentCloseCodes, so an expired-but-otherwise-valid session is
//     closed and the client transparently reconnects with a freshly
//     minted token. No frontend change is required: the existing
//     reconnect path already calls tokenProvider() on every connect.
//   - collabCloseSessionRevoked (4001) IS in the client's
//     PermanentCloseCodes ("auth failed / token rejected"), so a
//     revoked session, a vanished session record, or a device anomaly
//     tears the socket down for good and the client does not reconnect.
const (
	collabCloseReauthRequired = 4002
	collabCloseSessionRevoked = 4001
)

// collabUpgrader mirrors api/ws/handler.go's DefaultUpgrader. Auth
// already happened upstream so CheckOrigin is permissive; the JWT
// claim binds the connection to (workspace, user).
//
// Subprotocols advertises the "bearer" marker — when the browser
// client offers ["bearer", "<jwt>"] (see WebSocketBearerSubprotocol
// in api/middleware), the Upgrader echoes back "bearer" so the
// RFC 6455 handshake completes cleanly. AuthMiddleware has already
// validated the JWT at this point; the subprotocol echo is purely
// the protocol-negotiation half of the same flow.
var collabUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
	Subprotocols:    []string{middleware.WebSocketBearerSubprotocol},
}

// WithCollab wires the collaborative editor WS hub so the
// /api/documents/{id}/ws endpoint becomes available. When nil
// the endpoint responds 503 Service Unavailable, matching the
// nil-safe pattern of WithDocuments / WithSharing.
//
// The supplied hub must already be running (no Run() method —
// the hub operates inline on the WS goroutines, no background
// reactor needed). cmd/server's graceful-shutdown path should
// invoke hub.Shutdown() before draining the rest of the server
// so collab clients receive a clean close frame.
func (h *Handler) WithCollab(hub *collab.DocumentHub) *Handler {
	h.collab = hub
	return h
}

// WithCollabReauth wires the session checker + validator used to
// enforce a collab WebSocket's authorization for the full life of the
// connection (collabAuthPump). The same store backs AuthMiddleware's
// per-request revocation and per-session existence/device checks, so
// passing it here makes those guarantees hold on already-open sockets
// too — not just on the upgrade request.
//
// nil checker/validator (single-replica / dev mode without Redis)
// degrade to expiry-only enforcement: the socket is still torn down
// once its JWT expires (a local check needing no store), but
// out-of-band revocations are not observed mid-session. trustedProxyDepth
// must match AuthMiddleware's so the device-anomaly fingerprint IP is
// read from the same proxy hop.
func (h *Handler) WithCollabReauth(checker middleware.SessionChecker, validator middleware.SessionValidator, trustedProxyDepth int) *Handler {
	h.collabReauthChecker = checker
	h.collabReauthValidator = validator
	h.collabTrustedProxyDepth = trustedProxyDepth
	return h
}

// ServeDocumentCollab upgrades an HTTP request to a WebSocket and
// joins the resulting client to its document's room in the collab
// hub. The full handshake is:
//
//  1. Extract (workspaceID, userID) from JWT context — both must
//     be present; returns 401 otherwise.
//  2. Parse documentID from path — 400 on parse failure.
//  3. Fetch document metadata (folder + name + collab_mode) via
//     GetMetadata — NOT GetByID, since the binary state is sent
//     separately via the snapshot bundle and would otherwise be
//     read twice. 404 if the doc doesn't exist; 503 if the
//     documents service isn't wired.
//  4. Reject documents whose collab_mode is 'disabled' with 409
//     (the WS upgrade would otherwise succeed only for every
//     subsequent frame to be silently dropped — failing the
//     upgrade is clearer to operators and SDK authors).
//  5. Check folder-level permissions: RoleViewer is required to
//     observe; RoleEditor unlocks the write path. Read-only
//     viewers connect successfully but their inbound frames
//     are dropped server-side (CanWrite=false on the
//     DocumentClient). 403 if the caller lacks viewer.
//  6. Fetch the snapshot bundle (y_state + tail deltas) via
//     documents.Snapshot — this is the one heavy read on the
//     upgrade path. We do it AFTER the permission check so an
//     unauthorized caller pays only the lightweight GetMetadata
//     cost (mirrors the optimization we did on GetDocumentSnapshot
//     in P2a).
//  7. Upgrade the connection. After this point we cannot return
//     an HTTP status — all errors must be communicated via close
//     frames.
//  8. Register the client with the hub and push the snapshot
//     bundle as a single SyncStepUpdates frame.
//  9. Start read + write pumps. Both exit on connection close;
//     the read pump unregisters on exit so the room cleans up.
//
// The endpoint is mounted OUTSIDE TenantGuard (matching /ws) but
// inside AuthMiddleware. The workspaceID is sourced from JWT
// claims, the documentID from the URL.
func (h *Handler) ServeDocumentCollab(w http.ResponseWriter, r *http.Request) {
	if h.documents == nil || h.collab == nil {
		middleware.RespondError(w, http.StatusServiceUnavailable, middleware.ErrCodeUnsupportedOp, "documents disabled")
		return
	}
	workspaceID, ok := middleware.WorkspaceIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		middleware.RespondError(w, http.StatusUnauthorized, middleware.ErrCodeAuthMissingToken, "unauthenticated")
		return
	}
	documentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeBadRequest, "invalid document id")
		return
	}

	// Lightweight metadata lookup: gives us the parent folder
	// (for the permission check + capability resolution) and the
	// document's collab_mode (for the disabled-tombstone gate).
	// GetMetadata skips the binary y_state / y_state_vector
	// columns — the heavy snapshot read happens after auth passes.
	doc, parent, err := h.documents.GetMetadata(r.Context(), workspaceID, documentID)
	if err != nil {
		writeDocumentError(w, r, err)
		return
	}
	if doc.CollabMode == document.CollabModeDisabled {
		// Collab tombstone — refuse the upgrade with 409. The
		// client must explicitly flip the doc's mode via PATCH
		// /documents/{id}/collab-mode first. Returning 409
		// instead of an opaque "upgrade rejected" surfaces the
		// reason to the SDK author.
		middleware.RespondError(w, http.StatusConflict, middleware.ErrCodeConflict, "document collab is disabled")
		return
	}

	// Permission gate: viewer to connect, editor to write. The
	// CanWrite flag we hand to the hub captures the result of a
	// SECOND permission check at RoleEditor — a viewer-only user
	// still gets to JOIN the room (to observe peer updates) but
	// the hub silently drops their SyncUpdate / awareness frames.
	if err := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleViewer); err != nil {
		writeServiceError(w, r, err)
		return
	}
	canWrite := h.assertResourceAccess(r.Context(), permission.ResourceFolder, doc.FolderID, permission.RoleEditor) == nil

	// Snapshot the authorization facts BEFORE the upgrade hijacks the
	// connection: the JWT claims (expiry + sid) and the device-
	// fingerprint inputs (UA + proxy-resolved client IP). collabAuthPump
	// uses these to keep enforcing the authorization AuthMiddleware
	// checked once at upgrade, for the full life of the socket.
	authClaims, _ := middleware.ClaimsFromContext(r.Context())
	authUserAgent := r.UserAgent()
	authClientIP := collabClientIP(r, h.collabTrustedProxyDepth)

	// Snapshot bundle: y_state + tail deltas, used as the cold-
	// open payload pushed to the new client immediately after
	// the upgrade succeeds. We fetch BEFORE the upgrade so a
	// transient DB error surfaces as a clean 5xx HTTP response
	// rather than a half-open WS connection.
	snap, err := h.documents.Snapshot(r.Context(), workspaceID, documentID)
	if err != nil {
		writeDocumentError(w, r, err)
		return
	}

	conn, err := collabUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response on failure.
		return
	}

	client := collab.NewClient(
		workspaceID,
		userID,
		documentID,
		canWrite,
		collab.FromDocumentCapability(document.ResolveCapability(parent.EncryptionMode)),
	)

	// Push the snapshot bundle BEFORE any other frame so the
	// client's Y.Doc is initialized before it sees peer updates.
	// Convert []*document.Delta to [][]byte for the assembler.
	tailPayloads := make([][]byte, 0, len(snap.TailDeltas))
	for _, d := range snap.TailDeltas {
		tailPayloads = append(tailPayloads, d.Payload)
	}
	// RegisterWithSnapshot atomically enqueues the snapshot frame
	// AND inserts the client into the room under the same lock,
	// guaranteeing the snapshot is the first frame in client.send
	// even if peers are concurrently broadcasting updates. Using
	// separate Register + SendSnapshot would open a race window
	// where a peer's SyncUpdate could land in the FIFO ahead of
	// the snapshot, breaking the Y.Doc baseline contract.
	h.collab.RegisterWithSnapshot(client, snap.Document.YState, tailPayloads)

	logger := slog.Default().With(
		"subsystem", "collab",
		"workspace_id", workspaceID.String(),
		"user_id", userID.String(),
		"document_id", documentID.String(),
		"can_write", canWrite,
	)
	logger.Info("collab client connected", "tail_deltas", len(snap.TailDeltas))

	// Read + write pumps run on separate goroutines. The write
	// pump owns the conn for data writes (gorilla/websocket
	// requires single-writer serialization for WriteMessage /
	// NextWriter). The read pump owns reads and is permitted to
	// call WriteControl concurrently — gorilla/websocket
	// documents WriteControl + Close as safe to invoke
	// concurrently with any other method, which we rely on for
	// the close-frame paths in collabReadPump. Both pumps exit
	// when the connection closes or the hub unregisters the
	// client.
	go collabWritePump(client, conn, logger)
	go collabReadPump(h.collab, client, conn, logger)

	// Enforce the connection's authorization for its full lifetime.
	// Without this, the socket outlives its JWT and survives session
	// revocation. Started only when there is something to enforce:
	// always when the token carries an expiry, plus whenever a
	// revocation checker/validator is wired.
	if authClaims != nil {
		st := collabAuthState{
			workspaceID: workspaceID,
			userID:      userID,
			sessionID:   authClaims.SessionID,
			userAgent:   authUserAgent,
			clientIP:    authClientIP,
		}
		if authClaims.IssuedAt != nil {
			st.issuedAt = authClaims.IssuedAt.Time
		}
		if authClaims.ExpiresAt != nil {
			st.expiresAt = authClaims.ExpiresAt.Time
			st.hasExpiry = true
		}
		if st.hasExpiry || h.collabReauthChecker != nil || h.collabReauthValidator != nil {
			go collabAuthPump(client, conn, st, h.collabReauthChecker, h.collabReauthValidator, collabReauthInterval, logger)
		}
	}
}

// collabReadPump pulls binary frames off the connection, decodes
// them, and dispatches via hub.Handle. Exit conditions:
//
//   - Read deadline expired (client died / network stall) — clean
//     up via Unregister and let the write pump notice c.done.
//   - Malformed frame — close the connection with code 1003
//     (Unsupported Data) so the client doesn't auto-reconnect.
//   - Policy violation (ErrUnauthorizedWrite, ErrCollabDisabled,
//     ErrPresenceNotAllowed) — for the disabled case we close
//     with 1008 (Policy Violation) because the connection can
//     never become useful; for the other two we just drop the
//     frame and keep listening (a viewer-only client may still
//     receive peer updates).
func collabReadPump(hub *collab.DocumentHub, c *collab.DocumentClient, conn *websocket.Conn, logger *slog.Logger) {
	defer func() {
		hub.Unregister(c)
		_ = conn.Close()
	}()
	conn.SetReadLimit(collabMaxMessageSize)
	_ = conn.SetReadDeadline(time.Now().Add(collabPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(collabPongWait))
	})
	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			if !errors.Is(err, websocket.ErrCloseSent) &&
				!websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logger.Error("collab read failed", "err", err)
			}
			return
		}
		if msgType != websocket.BinaryMessage {
			// Yjs frames are binary. A TextMessage is either a
			// misconfigured client or a probe. Close with 1003
			// so the client gets an explicit reason.
			logger.Warn("collab non-binary frame received, closing", "msg_type", msgType)
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "binary frames only"),
				time.Now().Add(collabWriteWait),
			)
			return
		}
		frame, err := collab.DecodeFrame(raw)
		if err != nil {
			logger.Warn("collab frame decode failed", "err", err, "bytes", len(raw))
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "malformed frame"),
				time.Now().Add(collabWriteWait),
			)
			return
		}
		if err := hub.Handle(context.Background(), c, frame); err != nil {
			switch {
			case errors.Is(err, collab.ErrCollabDisabled):
				logger.Info("collab document disabled mid-session, closing")
				_ = conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "document is disabled"),
					time.Now().Add(collabWriteWait),
				)
				return
			case errors.Is(err, collab.ErrUnauthorizedWrite),
				errors.Is(err, collab.ErrPresenceNotAllowed):
				// Drop the frame silently from the client's
				// perspective; log for the operator. We don't
				// disconnect because a read-only or strict_zk
				// client still needs to receive peer updates.
				logger.Debug("collab inbound frame dropped", "reason", err.Error())
			default:
				// Persistence or unexpected error. Log it and
				// keep the connection open — a transient DB
				// blip shouldn't kill an editor session. The
				// client will retry the update on the next
				// keystroke.
				logger.Error("collab handle failed", "err", err)
			}
		}
	}
}

// collabWritePump drains the client's outbound queue and writes
// binary frames to the connection. Exits when c.Done() closes
// (hub unregistered the client) or any write fails. Pings the
// peer on every collabPingPeriod tick to keep middleboxes from
// idling the TCP connection out.
func collabWritePump(c *collab.DocumentClient, conn *websocket.Conn, logger *slog.Logger) {
	ticker := time.NewTicker(collabPingPeriod)
	defer func() {
		ticker.Stop()
		_ = conn.Close()
	}()
	for {
		select {
		case <-c.Done():
			_ = conn.SetWriteDeadline(time.Now().Add(collabWriteWait))
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		case msg, ok := <-c.Send():
			if !ok {
				// Should never happen — the hub never closes
				// the send channel. Defensive: bail out.
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(collabWriteWait))
			if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				logger.Debug("collab write failed", "err", err)
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(collabWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logger.Debug("collab ping failed", "err", err)
				return
			}
		}
	}
}

// collabAuthState is the immutable snapshot of a collab connection's
// authorization, captured at upgrade time. The connection was
// authenticated once by AuthMiddleware before the upgrade;
// collabAuthPump replays these facts against the token clock and the
// session store to keep enforcing that authorization for the life of
// the (otherwise indefinitely long-lived) socket.
type collabAuthState struct {
	workspaceID uuid.UUID
	userID      uuid.UUID
	sessionID   string
	issuedAt    time.Time
	expiresAt   time.Time
	hasExpiry   bool
	userAgent   string
	clientIP    string
}

// collabAuthDecision is the outcome of one auth re-check. Exactly one
// of {closeConn, transient!=nil, zero-value} is meaningful: closeConn
// means tear the socket down with code/reason; a non-nil transient is
// a store error that must NOT close the connection (logged, retried);
// the zero value means the authorization still holds.
type collabAuthDecision struct {
	closeConn bool
	code      int
	reason    string
	transient error
}

// evaluateCollabAuth re-checks a live collab connection's
// authorization. The order mirrors AuthMiddleware: expiry first, then
// the per-user revocation cutoff, then per-session existence + device
// anomaly. It differs from the HTTP path in one deliberate way — a
// transient store error returns a non-fatal `transient` rather than
// failing closed: tearing down an active editing session on a momentary
// Redis blip is far more disruptive than failing one retriable HTTP
// request, and token expiry (the core property) is enforced locally
// above regardless of store health.
func evaluateCollabAuth(ctx context.Context, st collabAuthState, checker middleware.SessionChecker, validator middleware.SessionValidator) collabAuthDecision {
	// 1. Token expiry — local, needs no store, so it holds even during
	//    a total session-store outage. A socket must not outlive its JWT.
	if st.hasExpiry && time.Now().After(st.expiresAt) {
		return collabAuthDecision{closeConn: true, code: collabCloseReauthRequired, reason: "token expired"}
	}
	// 2. Out-of-band revocation (logout, password reset, admin force-
	//    sign-out) via the per-user issued-at cutoff.
	if checker != nil {
		cctx, cancel := context.WithTimeout(ctx, middleware.SessionCheckTimeout)
		revoked, err := checker.IsRevoked(cctx, st.workspaceID, st.userID, st.issuedAt)
		cancel()
		switch {
		case err != nil:
			return collabAuthDecision{transient: err}
		case revoked:
			return collabAuthDecision{closeConn: true, code: collabCloseSessionRevoked, reason: "session revoked"}
		}
	}
	// 3. Per-session existence + device anomaly (6.2) for tokens that
	//    carry a sid. ErrSessionNotFound is the per-session revocation
	//    signal (DELETE /sessions/:id, logout, natural expiry).
	if validator != nil && st.sessionID != "" {
		vctx, cancel := context.WithTimeout(ctx, middleware.SessionCheckTimeout)
		verr := validator.ValidateSession(vctx, st.workspaceID, st.sessionID, st.userAgent, st.clientIP)
		cancel()
		switch {
		case verr == nil:
		case errors.Is(verr, session.ErrSessionNotFound):
			return collabAuthDecision{closeConn: true, code: collabCloseSessionRevoked, reason: "session revoked"}
		case errors.Is(verr, session.ErrSessionAnomaly):
			return collabAuthDecision{closeConn: true, code: collabCloseSessionRevoked, reason: "device changed"}
		default:
			return collabAuthDecision{transient: verr}
		}
	}
	return collabAuthDecision{}
}

// collabAuthPump enforces the connection's authorization for the life
// of the socket. It runs as a third goroutine alongside the read/write
// pumps; gorilla/websocket documents Close and WriteControl as safe to
// call concurrently with all other methods, which is what we rely on
// here (the read pump's close-frame paths rely on the same guarantee).
// Closing the conn unblocks the read pump's ReadMessage, which
// unregisters the client and lets the write pump exit too. The pump
// itself exits when the client is unregistered (c.Done()).
func collabAuthPump(c *collab.DocumentClient, conn *websocket.Conn, st collabAuthState, checker middleware.SessionChecker, validator middleware.SessionValidator, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.Done():
			return
		case <-ticker.C:
			d := evaluateCollabAuth(context.Background(), st, checker, validator)
			if d.transient != nil {
				logger.Warn("collab reauth check failed; keeping session open", "err", d.transient)
				continue
			}
			if d.closeConn {
				logger.Info("collab authorization no longer valid, closing", "code", d.code, "reason", d.reason)
				_ = conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(d.code, d.reason),
					time.Now().Add(collabWriteWait),
				)
				_ = conn.Close()
				return
			}
		}
	}
}

// collabClientIP resolves the client IP for the collab device-anomaly
// fingerprint from the same proxy hop AuthMiddleware uses, returning ""
// when it cannot be determined (the validator treats "" as "skip the
// IP half of the fingerprint").
func collabClientIP(r *http.Request, trustedProxyDepth int) string {
	if ip := middleware.ClientIPFromRequest(r, trustedProxyDepth); ip != nil {
		return ip.String()
	}
	return ""
}
