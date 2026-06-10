package iamcore

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	apimw "github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/tracing"
	"github.com/kennguy3n/zk-drive/internal/user"
)

// Provider is the value written to users.auth_provider for accounts
// federated from iam-core. It namespaces the (provider, subject) pair
// so an iam-core subject never collides with a Google/Microsoft SSO
// subject that happens to share the same string.
const Provider = "iamcore"

// principalCacheTTL bounds how long a resolved (subject -> local
// identity) mapping is trusted without re-reading the database. It
// trades a small staleness window for eliminating a per-request user
// lookup: at steady state a tenant's tokens resolve from memory, and
// role changes / deactivations performed in zk-drive propagate within
// one TTL. Token-level revocation remains iam-core's responsibility
// (access tokens are short-lived), so this cache never extends a
// session past the token's own expiry.
const principalCacheTTL = 60 * time.Second

// errAccountDeactivated is returned internally when a resolved user has
// been deactivated in zk-drive; the handler maps it to 403.
var errAccountDeactivated = errors.New("iamcore: account deactivated")

// principal is the resolved local identity for an iam-core subject,
// cached for principalCacheTTL.
type principal struct {
	userID      uuid.UUID
	workspaceID uuid.UUID
	role        string
	expiresAt   time.Time
}

// Middleware validates iam-core access tokens on every /api/* request
// and binds the resolved tenant/workspace/user identity onto the
// request context exactly as the built-in AuthMiddleware does, so all
// downstream handlers, guards, and the row-level-security GUC behave
// identically regardless of which identity provider authenticated the
// caller.
//
// On first contact from a tenant it auto-provisions a workspace (via
// TenantMapper) and a local user row (passwordless, federated), then
// caches the mapping. It is safe for concurrent use.
type Middleware struct {
	verifier *Verifier
	tenants  *TenantMapper
	users    *user.Service
	audit    *audit.Service

	cache sync.Map // subject -> *principal
}

// NewMiddleware constructs the iam-core authentication middleware.
func NewMiddleware(verifier *Verifier, tenants *TenantMapper, users *user.Service) *Middleware {
	return &Middleware{verifier: verifier, tenants: tenants, users: users}
}

// WithAudit wires an audit service so workspace/user provisioning and
// logins are recorded. Returns the receiver for chaining.
func (m *Middleware) WithAudit(svc *audit.Service) *Middleware {
	m.audit = svc
	return m
}

// Handler is the net/http middleware. It rejects requests without a
// valid iam-core bearer token (401) and deactivated accounts (403),
// and otherwise forwards to next with an identity-bound context.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reuse the built-in auth path's extractor so the iam-core
		// front-end accepts the bearer token from exactly the same
		// transports: the Authorization header for ordinary API calls
		// and the Sec-WebSocket-Protocol list for browser WebSocket
		// upgrades (which cannot carry custom headers). Without the WS
		// fallback the realtime-sync and collab endpoints mounted behind
		// this middleware would 401 every browser connection.
		raw, ok := apimw.ExtractBearerToken(r)
		if !ok {
			apimw.RespondError(w, http.StatusUnauthorized, apimw.ErrCodeAuthMissingToken, "missing or malformed Authorization credentials")
			return
		}

		// The request context is NOT yet tenant-bound at this point
		// (this middleware runs before TenantGuard), so the RLS GUC is
		// unset and provisioning INSERTs are permitted. We deliberately
		// keep using this un-bound context for verification and
		// provisioning, then bind the resolved workspace onto a child
		// context for the downstream handler chain.
		ctx := r.Context()

		identity, err := m.verifier.Verify(ctx, raw)
		if err != nil {
			apimw.RespondError(w, http.StatusUnauthorized, apimw.ErrCodeAuthInvalidToken, "invalid token")
			return
		}

		p, err := m.resolve(ctx, r, identity)
		switch {
		case errors.Is(err, errAccountDeactivated):
			apimw.RespondError(w, http.StatusForbidden, apimw.ErrCodeForbidden, "account deactivated")
			return
		case errors.Is(err, ErrNoTenant):
			apimw.RespondError(w, http.StatusUnauthorized, apimw.ErrCodeAuthInvalidToken, "token has no tenant context")
			return
		case err != nil:
			apimw.RespondInternalError(w, r, "iamcore resolve identity", err)
			return
		}

		reqCtx := apimw.WithIdentity(ctx, p.userID, p.workspaceID, p.role)
		// Mirror the built-in auth middleware's observability
		// enrichment so logs and traces carry the resolved identity in
		// iam-core mode too: Enrich mutates the request-scoped logger
		// slot in place so the post-dispatch AccessLog frame (emitted
		// outside chi) sees the attributes, and SetSpanUser stamps the
		// OTel enduser.* attributes onto the active span.
		reqCtx = logging.Enrich(reqCtx,
			"workspace_id", p.workspaceID.String(),
			"user_id", p.userID.String(),
			"role", p.role,
		)
		tracing.SetSpanUser(reqCtx, p.userID.String(), p.workspaceID.String())
		next.ServeHTTP(w, r.WithContext(reqCtx))
	})
}

// resolve maps a verified iam-core identity to a local principal,
// provisioning the workspace and user on first sight and refreshing the
// cache when the entry is missing, expired, or the token's role no
// longer matches the cached role.
func (m *Middleware) resolve(ctx context.Context, r *http.Request, id Identity) (*principal, error) {
	role := id.MappedRole()
	if cached, ok := m.cache.Load(id.Subject); ok {
		p := cached.(*principal)
		if time.Now().Before(p.expiresAt) && p.role == role {
			return p, nil
		}
	}

	workspaceID, err := m.tenants.ResolveWorkspace(ctx, id.TenantID, id.OrgID, "")
	if err != nil {
		return nil, err
	}

	u, err := m.ensureUser(ctx, r, workspaceID, id, role)
	if err != nil {
		return nil, err
	}
	if u.DeactivatedAt != nil {
		return nil, errAccountDeactivated
	}

	// iam-core is authoritative for authorization: when the token's
	// role differs from the stored role, sync the local row so admin
	// listings and built-in authorization checks stay consistent.
	if u.Role != role {
		if err := m.users.UpdateRole(ctx, u.WorkspaceID, u.ID, role); err != nil && !errors.Is(err, user.ErrNotFound) {
			logging.FromContext(ctx).Error("iamcore update role failed", "user_id", u.ID, "err", err)
		}
	}

	// Best-effort last-login stamp. Only runs on a cache miss (i.e.
	// at most once per principalCacheTTL per user), so it does not add
	// a write to the steady-state request path.
	if err := m.users.UpdateLastLogin(ctx, u.ID, time.Now().UTC()); err != nil && !errors.Is(err, user.ErrNotFound) {
		logging.FromContext(ctx).Error("iamcore update last login failed", "user_id", u.ID, "err", err)
	}

	p := &principal{
		userID:      u.ID,
		workspaceID: u.WorkspaceID,
		role:        role,
		expiresAt:   time.Now().Add(principalCacheTTL),
	}
	m.cache.Store(id.Subject, p)
	return p, nil
}

// ensureUser resolves the local user row for an iam-core subject,
// provisioning a passwordless federated user on first login. A unique-
// violation from two concurrent first-logins for the same subject is
// resolved by re-reading the row the winning request inserted.
func (m *Middleware) ensureUser(ctx context.Context, r *http.Request, workspaceID uuid.UUID, id Identity, role string) (*user.User, error) {
	u, err := m.users.GetByAuthProvider(ctx, Provider, id.Subject)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, user.ErrNotFound) {
		return nil, err
	}

	created, err := m.users.CreateFederated(ctx, workspaceID, id.emailOrFallback(), id.Name, role, Provider, id.Subject)
	if err != nil {
		// Lost the race: another request created the row between our
		// lookup and insert. Re-read it.
		if existing, lookupErr := m.users.GetByAuthProvider(ctx, Provider, id.Subject); lookupErr == nil {
			return existing, nil
		}
		return nil, err
	}
	if m.audit != nil {
		actor := created.ID
		m.audit.LogAction(ctx, workspaceID, &actor, audit.ActionSSOLogin, "", nil, r, map[string]any{
			"provider": Provider,
			"email":    created.Email,
			"result":   "provisioned",
		})
	}
	return created, nil
}

// emailOrFallback returns the identity's email, or a synthetic
// address derived from the subject when iam-core omitted the email
// claim. The users table requires a non-empty email; the synthetic
// form keeps it unique per subject and clearly marks the source.
func (id Identity) emailOrFallback() string {
	if id.Email != "" {
		return id.Email
	}
	return id.Subject + "@iamcore.local"
}

