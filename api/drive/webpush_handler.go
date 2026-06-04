package drive

import (
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/notification"
)

// maxPushBodyBytes bounds the request body the push subscribe /
// unsubscribe handlers will read. The real payload is tiny (an endpoint
// URL plus two short base64url keys), so 8 KiB is generous headroom
// while stopping an authenticated client from forcing the JSON decoder
// to allocate an arbitrarily large body (memory DoS) before the
// field-level length checks run. Mirrors the MaxBytesReader guard the
// Stripe webhook handler already uses (internal/billing/stripe.go).
const maxPushBodyBytes = 8 << 10

// pushSubscriptionRequest mirrors the JSON produced by the browser's
// PushSubscription.toJSON(): a push-service endpoint plus the ECDH
// (p256dh) and auth keys. The frontend POSTs this verbatim after
// pushManager.subscribe() resolves.
type pushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushUnsubscribeRequest carries the endpoint of the subscription to
// remove. Sent in the DELETE body so the value never lands in server
// logs / proxy access logs the way a query string would.
type pushUnsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

// VAPIDPublicKey returns the server's VAPID public key so the frontend
// can pass it as applicationServerKey to pushManager.subscribe. When
// web push is disabled (no VAPID keys configured) it responds 501 so
// the client can skip the subscription flow entirely.
func (h *Handler) VAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	if h.webpush == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "web push not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"public_key": h.webpush.PublicKey(),
	})
}

// SubscribePush registers (or refreshes) the caller's browser push
// subscription so they receive notifications while no tab / WebSocket
// is connected.
func (h *Handler) SubscribePush(w http.ResponseWriter, r *http.Request) {
	if h.webpush == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "web push not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)
	var req pushSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "endpoint, keys.p256dh and keys.auth are required")
		return
	}
	if err := h.webpush.Subscribe(r.Context(), workspaceID, userID, notification.PushSubscription{
		Endpoint: req.Endpoint,
		P256dh:   req.Keys.P256dh,
		Auth:     req.Keys.Auth,
	}); err != nil {
		writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UnsubscribePush removes the caller's browser push subscription
// identified by its endpoint. Idempotent: removing an unknown endpoint
// still returns 204.
func (h *Handler) UnsubscribePush(w http.ResponseWriter, r *http.Request) {
	if h.webpush == nil {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "web push not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)
	var req pushUnsubscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return
	}
	if req.Endpoint == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "endpoint is required")
		return
	}
	if err := h.webpush.Unsubscribe(r.Context(), workspaceID, userID, req.Endpoint); err != nil {
		writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
