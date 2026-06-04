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
// failClosed selects the posture on a suspension-lookup error:
//   - false (default): fail OPEN — the request proceeds, since
//     suspension is an availability control rather than a security
//     boundary and a transient DB blip must not lock the entire fleet
//     out.
//   - true: fail CLOSED — the request is rejected with 503. Intended
//     for deployments that use suspension for compliance / legal holds,
//     where a suspended workspace must never transact even during a
//     database outage. The body distinguishes "can't confirm" from a
//     confirmed suspension so platform tooling can tell them apart.
func SuspensionGuard(checker WorkspaceSuspensionChecker, failClosed bool) func(http.Handler) http.Handler {
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
				if failClosed {
					logging.FromContext(r.Context()).Warn("suspension lookup failed; rejecting request (fail-closed)",
						"workspace_id", workspaceID, "err", err)
					writeSuspensionUnavailable(w)
					return
				}
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
	// Match the nosniff defense WriteJSON applies to every other JSON
	// response so this hand-written body (a deliberately distinct
	// envelope) isn't the lone response a browser could MIME-sniff.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(suspendedWorkspaceBody{Error: "workspace_suspended", Reason: reason})
}

// writeSuspensionUnavailable is the fail-CLOSED response when the
// suspension lookup itself errored. It is deliberately distinct from
// writeSuspended ("workspace_suspended"): the workspace's suspension
// state is unknown, not confirmed-suspended, so platform tooling and
// clients can distinguish "blocked because we cannot verify" from
// "blocked because suspended". A shorter Retry-After reflects that this
// is expected to clear as soon as the lookup backend recovers.
func writeSuspensionUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(suspendedWorkspaceBody{Error: "suspension_check_unavailable"})
}
