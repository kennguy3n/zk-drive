package middleware

import (
	"net/http"
)

// AdminOnly returns a middleware that rejects any request where the
// caller's role (carried in the JWT claims) is not "admin". Must be
// composed after AuthMiddleware so the role is available in context.
func AdminOnly() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, ok := RoleFromContext(r.Context())
			if !ok || role != "admin" {
				RespondError(w, http.StatusForbidden, ErrCodeAdminOnly, "admin access required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
