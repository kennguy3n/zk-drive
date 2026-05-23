package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/tenantctx"
)

// TokenTTL is the default validity of issued session JWTs.
const TokenTTL = 24 * time.Hour

// MFAChallengeTokenTTL is the validity of the short-lived JWT
// issued by the login handler when the user has TOTP enrolled.
// The user has this long to complete the 2FA verify step before
// the challenge expires and they must re-enter their password.
// 5 minutes is the industry standard (GitHub, Stripe, AWS).
const MFAChallengeTokenTTL = 5 * time.Minute

// Purpose values for the Purpose claim.
const (
	// PurposeMFAChallenge marks a token issued by the password-
	// verify step that has NOT yet satisfied the second factor.
	// AuthMiddleware rejects any token with this purpose; only the
	// dedicated MFA verify endpoint accepts it.
	PurposeMFAChallenge = "mfa_challenge"
	// PurposeMFAEnroll marks a token issued when the workspace
	// requires MFA but the user has no credential yet. It grants
	// ONLY the enrollment endpoints — the user cannot reach any
	// data-plane handler until they finish enrollment and exchange
	// the enroll token for a full session token by verifying a
	// freshly generated TOTP code.
	PurposeMFAEnroll = "mfa_enroll"
)

// Claims is the JWT payload used by zk-drive.
//
// Purpose is empty for ordinary session tokens (the default).
// When non-empty, the token is restricted to a specific endpoint
// family (see PurposeMFAChallenge / PurposeMFAEnroll). AuthMiddleware
// rejects any token with a non-empty purpose so that an attacker who
// captures an mfa-challenge token cannot replay it against a data-
// plane endpoint.
type Claims struct {
	UserID      uuid.UUID `json:"user_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Role        string    `json:"role"`
	Purpose     string    `json:"purpose,omitempty"`
	jwt.RegisteredClaims
}

type contextKey string

const (
	claimsContextKey contextKey = "zkdrive.claims"
	userIDContextKey contextKey = "zkdrive.user_id"
	roleContextKey   contextKey = "zkdrive.role"
)

// workspaceID is stored via internal/tenantctx so the pgxpool
// PrepareConn hook in internal/database can read the same value
// without forming an import cycle on this package. The accessor
// below remains the public entrypoint for handler code so other
// packages don't grow a direct import on tenantctx.

// IssueToken signs and returns a new session JWT with zk-drive's
// standard claims. Purpose is left empty so AuthMiddleware accepts
// the token on every protected endpoint.
func IssueToken(secret string, userID, workspaceID uuid.UUID, role string, ttl time.Duration) (string, time.Time, error) {
	return issueWithPurpose(secret, userID, workspaceID, role, "", ttl)
}

// IssueMFAChallengeToken signs a short-lived JWT marked with
// purpose=mfa_challenge. AuthMiddleware rejects it; only the
// /auth/totp/verify endpoint will accept it as proof that the
// password factor has been satisfied.
func IssueMFAChallengeToken(secret string, userID, workspaceID uuid.UUID) (string, time.Time, error) {
	// Role is intentionally empty: a challenge token must not carry
	// authority on the data plane, and emitting the user's role
	// here would invite a future bug where some handler bypasses
	// AuthMiddleware's purpose check and treats the challenge as a
	// session.
	return issueWithPurpose(secret, userID, workspaceID, "", PurposeMFAChallenge, MFAChallengeTokenTTL)
}

// IssueMFAEnrollToken signs a short-lived JWT marked with
// purpose=mfa_enroll. It authorizes ONLY the enrollment endpoints
// for a user on a workspace that requires MFA but has not yet
// completed enrollment.
func IssueMFAEnrollToken(secret string, userID, workspaceID uuid.UUID) (string, time.Time, error) {
	return issueWithPurpose(secret, userID, workspaceID, "", PurposeMFAEnroll, MFAChallengeTokenTTL)
}

func issueWithPurpose(secret string, userID, workspaceID uuid.UUID, role, purpose string, ttl time.Duration) (string, time.Time, error) {
	if ttl == 0 {
		ttl = TokenTTL
	}
	now := time.Now().UTC()
	exp := now.Add(ttl)
	claims := &Claims{
		UserID:      userID,
		WorkspaceID: workspaceID,
		Role:        role,
		Purpose:     purpose,
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

// SessionChecker is consulted by AuthMiddleware on every authenticated
// request to honour out-of-band revocations (logout, password reset,
// admin force-sign-out) without rotating the JWT signing secret.
//
// IsRevoked returns true when a token with the given (workspaceID,
// userID, issuedAt) tuple has been revoked. Transport-level errors
// must be returned to the caller verbatim — the middleware fails
// closed on err != nil so a flaky Redis cannot silently degrade
// revocation to a no-op.
//
// `issuedAt` is the JWT's `iat` claim; implementations compare it
// against a per-user cutoff stored when the user logs out or has
// their sessions force-revoked.
type SessionChecker interface {
	IsRevoked(ctx context.Context, workspaceID, userID uuid.UUID, issuedAt time.Time) (bool, error)
}

// SessionCheckTimeout is the upper bound on how long AuthMiddleware
// will wait for a SessionChecker.IsRevoked call before giving up and
// failing closed.
//
// Healthy Redis (sub-millisecond latency) never approaches this
// bound, but a partial outage — packet loss, half-open TCP
// connections, an overloaded node — can leave a Redis call blocked
// indefinitely on the request's context. Without a bound, every
// authenticated request would hang for the full client read deadline,
// effectively taking down the API surface on a slow-Redis incident
// even though the request would have failed closed anyway.
//
// 1 second is comfortably above the p99.99 of every well-behaved
// Redis deployment we've measured, while still keeping the
// worst-case auth latency on a Redis incident inside the typical
// HTTP client timeout.
const SessionCheckTimeout = 1 * time.Second

// AuthMiddleware returns a middleware that validates a Bearer JWT in the
// Authorization header and injects the parsed identity into the request
// context. Requests without a valid token receive HTTP 401.
//
// When checker is non-nil, AuthMiddleware additionally calls
// checker.IsRevoked after signature/expiry validation passes; a
// revoked token (or any error from the checker) also yields HTTP 401.
// Passing nil keeps the previous stateless-JWT behaviour and is
// retained for tests and the (deprecated) in-memory deployment path
// where Redis isn't wired.
func AuthMiddleware(secret string, checker SessionChecker) func(http.Handler) http.Handler {
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
			// Purpose-scoped tokens (mfa_challenge, mfa_enroll) MUST
			// NOT reach data-plane endpoints. AuthMiddleware is the
			// single chokepoint: rejecting non-empty purpose here
			// means an attacker who captures a challenge token cannot
			// replay it against any handler other than the dedicated
			// MFA endpoints (which use MFAChallengeMiddleware /
			// MFAEnrollMiddleware below).
			if claims.Purpose != "" {
				http.Error(w, "token not valid for this endpoint", http.StatusUnauthorized)
				return
			}
			if checker != nil {
				// The JWT carries IssuedAt as a *jwt.NumericDate
				// (second-precision Unix time). When the claim is
				// missing entirely we treat the token as
				// pre-revocation-era: there's no iat to compare
				// against a cutoff, so we conservatively fail
				// closed.
				if claims.IssuedAt == nil {
					http.Error(w, "token missing iat", http.StatusUnauthorized)
					return
				}
				// Bound the IsRevoked call so a partial Redis
				// outage (half-open connections, packet loss)
				// cannot hang every authenticated request for
				// the client's full read deadline. We still fail
				// closed when the check errors — the timeout
				// just makes the failure fast and observable
				// instead of a silent stall. See
				// SessionCheckTimeout docs.
				checkCtx, cancel := context.WithTimeout(r.Context(), SessionCheckTimeout)
				revoked, ierr := checker.IsRevoked(checkCtx, claims.WorkspaceID, claims.UserID, claims.IssuedAt.Time)
				cancel()
				if ierr != nil {
					// Fail closed on store unreachable. Without
					// this the revocation guarantee would silently
					// vanish behind a single Redis outage.
					http.Error(w, "revocation check failed", http.StatusUnauthorized)
					return
				}
				if revoked {
					http.Error(w, "token revoked", http.StatusUnauthorized)
					return
				}
			}
			ctx := withClaims(r.Context(), claims)
			// Layer authenticated identity onto the request-scoped
			// logger so every handler-emitted log line carries the
			// tuple (request_id, workspace_id, user_id, role)
			// without each handler having to pass them through.
			// Skipped for unauthenticated requests (the bearer
			// guard above returns 401 before we reach this point),
			// so the attributes are never set to zero values that
			// would pollute aggregation queries.
			//
			// Enrich (not WithContext) is the right primitive here
			// because the auth middleware runs INSIDE chi, and the
			// access log line emitted by logging.AccessLog OUTSIDE
			// chi must also carry these identity attributes for
			// operator filtering. Enrich mutates the request-scoped
			// logger slot in place so the post-dispatch AccessLog
			// frame sees the enriched logger.
			ctx = logging.Enrich(ctx,
				"workspace_id", claims.WorkspaceID.String(),
				"user_id", claims.UserID.String(),
				"role", claims.Role,
			)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func withClaims(ctx context.Context, c *Claims) context.Context {
	ctx = context.WithValue(ctx, claimsContextKey, c)
	ctx = context.WithValue(ctx, userIDContextKey, c.UserID)
	ctx = tenantctx.WithWorkspaceID(ctx, c.WorkspaceID)
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
// request by auth / tenant middleware. It delegates to
// tenantctx.WorkspaceIDFromContext so handler code and the database
// layer agree on a single canonical context key.
func WorkspaceIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	return tenantctx.WorkspaceIDFromContext(ctx)
}

// WithWorkspaceID returns a child context tagged with workspaceID so
// downstream handlers (and the pgxpool PrepareConn hook that binds
// `app.workspace_id` for row-level-security policies) see the tenant
// scope. It is the public counterpart to WorkspaceIDFromContext and
// the canonical entry point for handler / service code that needs to
// attach a workspace id to a context produced outside the JWT auth
// path (e.g. service-to-service calls, internal admin tools, tests).
// It delegates to tenantctx.WithWorkspaceID to keep the api/middleware
// and internal/database packages on the same canonical context key.
func WithWorkspaceID(ctx context.Context, workspaceID uuid.UUID) context.Context {
	return tenantctx.WithWorkspaceID(ctx, workspaceID)
}

// RoleFromContext returns the authenticated user's role within the workspace.
func RoleFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(roleContextKey).(string)
	return v, ok
}

// PurposeMiddleware returns a middleware that accepts ONLY tokens
// whose Purpose claim matches `want`. Used by the MFA verify and
// MFA enrollment routes so that an mfa_challenge token cannot be
// used to access protected data, and a session token cannot be
// used to satisfy the 2FA verification step.
//
// Unlike AuthMiddleware, PurposeMiddleware does NOT consult the
// SessionChecker: challenge / enroll tokens are short-lived (5
// minutes), single-purpose, and not stored in the revocation
// index. Adding them would burn Redis traffic for zero security
// benefit since the token expires before any revocation window
// would be meaningful.
func PurposeMiddleware(secret, want string) func(http.Handler) http.Handler {
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
			if claims.Purpose != want {
				http.Error(w, "token not valid for this endpoint", http.StatusUnauthorized)
				return
			}
			ctx := withClaims(r.Context(), claims)
			ctx = logging.Enrich(ctx,
				"workspace_id", claims.WorkspaceID.String(),
				"user_id", claims.UserID.String(),
				"purpose", claims.Purpose,
			)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
