package middleware

import (
	"context"
	"net/http"
)

// PlatformPrincipal is the authenticated identity behind a platform
// API key. It is intentionally a tiny interface (rather than importing
// the concrete internal/platform.APIKey type) so this middleware stays
// decoupled from the control-plane service layer.
type PlatformPrincipal interface {
	// HasPermission reports whether the principal carries the named
	// capability (e.g. "tenant:write").
	HasPermission(permission string) bool
}

// PlatformAuthenticator validates a presented platform API key and
// returns the matching principal. Implementations live in the control
// plane (api/platform wires internal/platform.APIKeyStore); a non-nil
// error means the key is unknown, revoked, or malformed.
type PlatformAuthenticator interface {
	AuthenticateKey(ctx context.Context, presented string) (PlatformPrincipal, error)
}

type platformPrincipalCtxKey struct{}

// PlatformAuth authenticates requests with a platform API key supplied
// as `Authorization: Bearer pk_...`. It is a SEPARATE chain from the
// workspace JWT AuthMiddleware: platform endpoints are fleet-wide and
// must never accept a tenant user token, and tenant endpoints must
// never accept a platform key. On success it binds the principal to
// the request context for downstream RequirePlatformPermission checks.
func PlatformAuth(a PlatformAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := ExtractBearerToken(r)
			if !ok {
				RespondError(w, http.StatusUnauthorized, ErrCodeAuthMissingToken, "missing platform API key")
				return
			}
			principal, err := a.AuthenticateKey(r.Context(), token)
			if err != nil || principal == nil {
				RespondError(w, http.StatusUnauthorized, ErrCodeAuthInvalidToken, "invalid platform API key")
				return
			}
			ctx := context.WithValue(r.Context(), platformPrincipalCtxKey{}, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequirePlatformPermission gates a route on a specific capability. It
// must run after PlatformAuth; a principal lacking the permission gets
// 403 Forbidden.
func RequirePlatformPermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := PlatformPrincipalFromContext(r.Context())
			if !ok || principal == nil {
				RespondError(w, http.StatusUnauthorized, ErrCodeAuthMissingToken, "missing platform API key")
				return
			}
			if !principal.HasPermission(permission) {
				RespondError(w, http.StatusForbidden, ErrCodeForbidden, "platform API key lacks required permission")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// PlatformPrincipalFromContext returns the platform principal bound by
// PlatformAuth, if any.
func PlatformPrincipalFromContext(ctx context.Context) (PlatformPrincipal, bool) {
	p, ok := ctx.Value(platformPrincipalCtxKey{}).(PlatformPrincipal)
	return p, ok
}
