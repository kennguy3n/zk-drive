package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TokenTTL is the default validity of issued JWTs.
const TokenTTL = 24 * time.Hour

// Claims is the JWT payload used by zk-drive.
type Claims struct {
	UserID      uuid.UUID `json:"user_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Role        string    `json:"role"`
	jwt.RegisteredClaims
}

type contextKey string

const (
	claimsContextKey      contextKey = "zkdrive.claims"
	userIDContextKey      contextKey = "zkdrive.user_id"
	workspaceIDContextKey contextKey = "zkdrive.workspace_id"
	roleContextKey        contextKey = "zkdrive.role"
)

// IssueToken signs and returns a new JWT with zk-drive's standard claims.
func IssueToken(secret string, userID, workspaceID uuid.UUID, role string, ttl time.Duration) (string, time.Time, error) {
	if ttl == 0 {
		ttl = TokenTTL
	}
	now := time.Now().UTC()
	exp := now.Add(ttl)
	claims := &Claims{
		UserID:      userID,
		WorkspaceID: workspaceID,
		Role:        role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		return "", time.Time{}, err
	}
	return s, exp, nil
}

// ParseToken verifies the token signature and expiry and returns the parsed
// claims.
func ParseToken(secret, raw string) (*Claims, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

// AuthMiddleware returns a middleware that validates a Bearer JWT in the
// Authorization header and injects the parsed identity into the request
// context. Requests without a valid token receive HTTP 401.
func AuthMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" || !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			raw := strings.TrimPrefix(header, "Bearer ")
			claims, err := ParseToken(secret, raw)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			ctx := withClaims(r.Context(), claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func withClaims(ctx context.Context, c *Claims) context.Context {
	ctx = context.WithValue(ctx, claimsContextKey, c)
	ctx = context.WithValue(ctx, userIDContextKey, c.UserID)
	ctx = context.WithValue(ctx, workspaceIDContextKey, c.WorkspaceID)
	ctx = context.WithValue(ctx, roleContextKey, c.Role)
	return ctx
}

// ClaimsFromContext returns the parsed JWT claims, if any.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsContextKey).(*Claims)
	return c, ok
}

// UserIDFromContext returns the authenticated user's id.
func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(userIDContextKey).(uuid.UUID)
	return v, ok
}

// WorkspaceIDFromContext returns the workspace id bound to the current
// request by auth / tenant middleware.
func WorkspaceIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(workspaceIDContextKey).(uuid.UUID)
	return v, ok
}

// RoleFromContext returns the authenticated user's role within the workspace.
func RoleFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(roleContextKey).(string)
	return v, ok
}
