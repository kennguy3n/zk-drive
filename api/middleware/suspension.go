package middleware

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// WorkspaceSuspensionChecker reports whether a workspace has been
// suspended by the platform control plane. It is satisfied by
// *platform.PlatformService; the interface lives here so this package
// does not import the control-plane service.
type WorkspaceSuspensionChecker interface {
	WorkspaceSuspension(ctx context.Context, workspaceID uuid.UUID) (suspended bool, reason string, err error)
}

// suspendedWorkspaceBody is the exact JSON body returned for a request
// against a suspended workspace. It is deliberately distinct from the
// standard ErrorResponse envelope: suspension is an operational fleet
// state, and the contract (`{"error":"workspace_suspended","reason":...}`)
// is consumed by the platform tooling rather than the locale layer.
type suspendedWorkspaceBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"`
}

// SuspensionGuard returns 503 Service Unavailable for any request whose
// workspace has been suspended. It must run after AuthMiddleware (which
// binds the workspace id) and is a no-op when no workspace is bound or
// when checker is nil.
//
// It fails OPEN: if the suspension lookup errors (e.g. transient DB
// blip) the request proceeds, since suspension is an availability
// control rather than a security boundary and a lookup failure must not
// lock the entire fleet out.
func SuspensionGuard(checker WorkspaceSuspensionChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if checker == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			workspaceID, ok := WorkspaceIDFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			suspended, reason, err := checker.WorkspaceSuspension(r.Context(), workspaceID)
			if err != nil {
				logging.FromContext(r.Context()).Warn("suspension lookup failed; allowing request",
					"workspace_id", workspaceID, "err", err)
				next.ServeHTTP(w, r)
				return
			}
			if suspended {
				writeSuspended(w, reason)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeSuspended(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(suspendedWorkspaceBody{Error: "workspace_suspended", Reason: reason})
}
