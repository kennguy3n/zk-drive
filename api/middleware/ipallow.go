package middleware

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// IPBlockedHeader is set to "true" on the 403 response when the
// IPAllowlist middleware rejects a request, so clients (and edge
// log pipelines) can distinguish an allowlist denial from a generic
// 403 without parsing the JSON body.
const IPBlockedHeader = "X-ZkDrive-IP-Blocked"

// IPAllowChecker is the subset of workspace.IPAllowService the
// middleware needs. Declared as an interface so the middleware is
// unit-testable with a fake and so a nil service cleanly disables
// enforcement.
type IPAllowChecker interface {
	CheckAccess(ctx context.Context, workspaceID uuid.UUID, clientIP net.IP) error
}

// IPAllowlist returns a middleware that enforces a workspace's IP
// allowlist. It MUST be composed AFTER the auth/tenant middleware so
// the workspace id is already bound in the request context.
//
// When checker is nil — including a typed-nil concrete
// *workspace.IPAllowService passed through the interface — the
// middleware is a pass-through, so the server boots without an
// IPAllowService wired (mirroring the nil-service-disables-feature
// convention used elsewhere, e.g. admin.Handler.WithWebhooks).
//
// The client IP is resolved from X-Forwarded-For honouring
// trustedProxyDepth (the number of trusted proxies appended to the
// right of the header), falling back to the request's RemoteAddr.
// A denied request gets a 403 with the X-ZkDrive-IP-Blocked: true
// header; a service error (e.g. the DB is unreachable) fails closed
// with a 500 rather than silently admitting the request.
func IPAllowlist(checker IPAllowChecker, trustedProxyDepth int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if checker == nil {
			return next
		}
		// A typed-nil *IPAllowService satisfies the interface but is
		// not == nil; unwrap it so it disables enforcement instead of
		// panicking on a nil receiver inside CheckAccess.
		if svc, ok := checker.(*workspace.IPAllowService); ok && svc == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			workspaceID, ok := WorkspaceIDFromContext(r.Context())
			if !ok {
				// IPAllowlist is mounted after the tenant guard;
				// a missing workspace here is a wiring bug, not a
				// client error. Fail closed.
				RespondError(w, http.StatusInternalServerError, ErrCodeInternal, "workspace not in context")
				return
			}
			clientIP := ClientIPFromRequest(r, trustedProxyDepth)
			err := checker.CheckAccess(r.Context(), workspaceID, clientIP)
			switch {
			case err == nil:
				next.ServeHTTP(w, r)
			case errors.Is(err, workspace.ErrIPBlocked):
				w.Header().Set(IPBlockedHeader, "true")
				RespondError(w, http.StatusForbidden, ErrCodeIPBlocked, "access from this network is not allowed")
			default:
				RespondInternalError(w, r, "ip allowlist check", err)
			}
		})
	}
}

// ClientIPFromRequest resolves the client IP for allowlisting.
//
// trustedProxyDepth is the number of trusted reverse proxies that
// append to X-Forwarded-For. Each trusted proxy appends the address
// it received the connection from, so the real client address is the
// entry trustedProxyDepth positions from the right
// (index len(parts)-trustedProxyDepth). Entries further left are
// client-supplied and therefore spoofable, so they are ignored.
//
// When the header is absent, depth is <= 0, or the computed index is
// out of range (fewer hops than configured — a direct connection or
// misconfiguration), it falls back to the TCP peer in RemoteAddr,
// which cannot be spoofed. Returns nil only when no usable address
// can be parsed.
//
// All X-Forwarded-For header lines are joined before splitting, not
// just the first. Standard proxies merge XFF into one comma-separated
// value, but a client can send its own X-Forwarded-For and a proxy
// that appends a *separate* header line (rather than merging) would
// otherwise leave http.Header.Get reading only the client's spoofed
// line — so the right-anchored "trusted" entry would actually be
// attacker-controlled. Joining Values preserves hop order (each
// proxy's appended entry stays to the right of earlier ones), so the
// depth-from-right index still selects the closest trusted proxy.
func ClientIPFromRequest(r *http.Request, trustedProxyDepth int) net.IP {
	if trustedProxyDepth > 0 {
		if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
			parts := strings.Split(strings.Join(values, ","), ",")
			idx := len(parts) - trustedProxyDepth
			if idx >= 0 && idx < len(parts) {
				if ip := net.ParseIP(strings.TrimSpace(parts[idx])); ip != nil {
					return ip
				}
			}
		}
	}
	return ipFromRemoteAddr(r.RemoteAddr)
}

// ipFromRemoteAddr extracts the IP from a "host:port" RemoteAddr,
// tolerating a bare host (no port) and bracketed IPv6 literals.
func ipFromRemoteAddr(addr string) net.IP {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return net.ParseIP(host)
}
