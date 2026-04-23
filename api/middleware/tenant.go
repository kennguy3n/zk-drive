package middleware

import "net/http"

// TenantGuard ensures that the request has a workspace id bound by the auth
// middleware. Handlers downstream use WorkspaceIDFromContext to scope every
// database query to this id. Requests without a workspace id receive HTTP
// 401. This middleware must run after AuthMiddleware.
func TenantGuard() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := WorkspaceIDFromContext(r.Context()); !ok {
				http.Error(w, "missing workspace context", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
