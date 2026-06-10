package drive

import (
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/notification"
)

// deviceTokenRequest is the JSON the native apps POST to register (and
// DELETE to unregister) a push device token. `platform` is "ios" (APNs)
// or "android" (FCM); `token` is the opaque device token the platform
// push service issued for this app install.
type deviceTokenRequest struct {
	Platform string `json:"platform"`
	Token    string `json:"token"`
}

// RegisterDevice registers (or refreshes) the caller's native push
// device token so the server can deliver notifications to the phone
// while the app is backgrounded or killed. The mobile counterpart to
// SubscribePush (browser Web Push).
//
// Responds 501 when mobile push is unconfigured (no APNs / FCM provider)
// or when the token's platform has no configured provider — so the app
// learns push won't work instead of silently registering a token nothing
// can deliver to.
func (h *Handler) RegisterDevice(w http.ResponseWriter, r *http.Request) {
	if !h.mobilePush.Enabled() {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "mobile push not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	dt, ok := decodeDeviceTokenRequest(w, r)
	if !ok {
		return
	}
	if err := h.mobilePush.Register(r.Context(), workspaceID, userID, dt); err != nil {
		writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UnregisterDevice removes the caller's native push device token,
// identified by platform + token. Idempotent: removing an unknown token
// still returns 204. The app calls this on sign-out or when the user
// disables notifications.
func (h *Handler) UnregisterDevice(w http.ResponseWriter, r *http.Request) {
	if !h.mobilePush.Enabled() {
		middleware.RespondError(w, http.StatusNotImplemented, middleware.ErrCodeUnsupportedOp, "mobile push not configured")
		return
	}
	workspaceID, _ := middleware.WorkspaceIDFromContext(r.Context())
	userID, _ := middleware.UserIDFromContext(r.Context())

	dt, ok := decodeDeviceTokenRequest(w, r)
	if !ok {
		return
	}
	if err := h.mobilePush.Unregister(r.Context(), workspaceID, userID, dt); err != nil {
		writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeDeviceTokenRequest reads and minimally validates the shared
// register/unregister body, writing the 400 response itself and
// returning ok=false on a malformed body or missing field. Deeper
// validation (known platform, token length) happens in the service so
// the rules live in one place.
func decodeDeviceTokenRequest(w http.ResponseWriter, r *http.Request) (notification.DeviceToken, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)
	var req deviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMalformedJSON, "invalid json body")
		return notification.DeviceToken{}, false
	}
	if req.Platform == "" || req.Token == "" {
		middleware.RespondError(w, http.StatusBadRequest, middleware.ErrCodeMissingField, "platform and token are required")
		return notification.DeviceToken{}, false
	}
	return notification.DeviceToken{
		Platform: notification.Platform(req.Platform),
		Token:    req.Token,
	}, true
}
