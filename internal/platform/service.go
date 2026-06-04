package platform

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/fabric"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// suspensionCacheTTL bounds the lifetime of a cached suspension
// snapshot. It is short because SuspensionGuard sits on every
// authenticated request: the cache exists to collapse the per-request
// DB round trip into one read per workspace per TTL, not to be the
// source of truth. Every suspend/resume busts the entry, so this TTL
// is only the fallback window if a bust fails (a Redis blip) — and
// suspension is an availability control, not a security boundary, so a
// brief staleness is acceptable. Mirrors the IP-allowlist snapshot TTL.
const suspensionCacheTTL = 30 * time.Second

// suspensionCacheKeyPrefix namespaces the per-workspace suspension
// snapshot keys in Redis.
const suspensionCacheKeyPrefix = "platform:suspension"

// suspensionSnapshot is the cached representation of a workspace's
// suspension state served on the SuspensionGuard hot path.
type suspensionSnapshot struct {
	Suspended bool   `json:"suspended"`
	Reason    string `json:"reason,omitempty"`
}

// sessionRevokeTTL bounds how long a per-user revocation cutoff lives
// in the session store. It matches the API's default token lifetime
// (middleware.TokenTTL is 24h): after that no token the cutoff could
// reject is still valid. Declared locally so this internal package
// does not import api/middleware.
const sessionRevokeTTL = 24 * time.Hour

// SessionRevoker is the subset of the Redis session store
// SuspendWorkspace needs to forcibly log out a tenant's users. Both
// methods are satisfied by *session.RedisSessionStore.
type SessionRevoker interface {
	RevokeAllForUser(ctx context.Context, workspaceID, userID uuid.UUID) error
	RevokeUser(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time, ttl time.Duration) error
}

// TenantProvisioner is the subset of *fabric.Provisioner used to mint
// per-workspace object-storage tenants at provision time.
type TenantProvisioner interface {
	Provision(ctx context.Context, workspaceID uuid.UUID, workspaceName string) (*fabric.Credentials, error)
}

// WelcomeEmail is the payload handed to a WelcomeMailer when a
// workspace is provisioned through the platform API.
type WelcomeEmail struct {
	WorkspaceID   uuid.UUID
	WorkspaceName string
	OwnerEmail    string
	OwnerName     string
	// TempPassword is the generated initial password for the owner
	// account. The mailer is responsible for delivering it (or a
	// password-reset link) securely.
	TempPassword string
}

// WelcomeMailer delivers the provisioning welcome message. Optional:
// when unwired, ProvisionWorkspace skips the send.
type WelcomeMailer interface {
	SendWorkspaceWelcome(ctx context.Context, in WelcomeEmail) error
}

// AlertDispatcher fires the notification channels for a triggered
// usage-alert rule. Optional: when unwired, EvaluateUsageAlerts still
// reports firings but marks the channels as not dispatched.
type AlertDispatcher interface {
	DispatchWebhook(ctx context.Context, url string, firing AlertFiring) error
	DispatchEmail(ctx context.Context, email string, firing AlertFiring) error
}

// SubscriptionInspector resolves the upstream (Stripe) subscription
// state for a customer so BulkReconcileBilling can compare it against
// the locally-stored plan tier. Optional: when unwired, reconciliation
// falls back to structural checks (paid local tier without a linked
// customer, linked customer it cannot verify).
type SubscriptionInspector interface {
	// SubscriptionStatus returns the subscription status (e.g.
	// "active", "canceled") and the tier it maps to. An empty status
	// means "no subscription found for this customer".
	SubscriptionStatus(ctx context.Context, stripeCustomerID string) (status string, tier string, err error)
}

// PlatformService is the control-plane service. Its only required
// dependencies are the pool plus the workspace / user / billing
// services; everything else (session revocation, fabric provisioning,
// welcome email, alert dispatch, Stripe inspection) is optional and
// wired via the With* setters so the service degrades gracefully when
// a subsystem is disabled.
type PlatformService struct {
	pool       *pgxpool.Pool
	workspaces *workspace.Service
	users      *user.Service
	billing    *billing.Service

	sessions      SessionRevoker
	provisioner   TenantProvisioner
	mailer        WelcomeMailer
	dispatcher    AlertDispatcher
	subscriptions SubscriptionInspector

	rdb    redis.UniversalClient
	clock  func() time.Time
	logger *slog.Logger
}

// NewService constructs a PlatformService. pool, workspaces, users and
// billing are required; pass the optional collaborators via With*.
func NewService(pool *pgxpool.Pool, workspaces *workspace.Service, users *user.Service, billing *billing.Service) *PlatformService {
	return &PlatformService{
		pool:       pool,
		workspaces: workspaces,
		users:      users,
		billing:    billing,
		clock:      time.Now,
		logger:     slog.Default(),
	}
}

// WithSuspensionCache wires a Redis client used to cache workspace
// suspension state on the hot SuspensionGuard path, which runs on
// every authenticated request. rdb may be nil (caching disabled —
// every WorkspaceSuspension reads through to Postgres). The cache is a
// pure accelerator: any Redis error falls through to the authoritative
// store, and every suspend/resume busts the entry so a state change
// takes effect immediately rather than after the TTL.
func (s *PlatformService) WithSuspensionCache(rdb redis.UniversalClient) *PlatformService {
	s.rdb = rdb
	return s
}

// WithSessions wires the session store used to revoke a suspended
// workspace's active sessions.
func (s *PlatformService) WithSessions(r SessionRevoker) *PlatformService {
	s.sessions = r
	return s
}

// WithProvisioner wires the fabric tenant provisioner used at
// workspace-provision time.
func (s *PlatformService) WithProvisioner(p TenantProvisioner) *PlatformService {
	s.provisioner = p
	return s
}

// WithMailer wires the welcome-email sender.
func (s *PlatformService) WithMailer(m WelcomeMailer) *PlatformService {
	s.mailer = m
	return s
}

// WithAlertDispatcher wires the usage-alert notification dispatcher.
func (s *PlatformService) WithAlertDispatcher(d AlertDispatcher) *PlatformService {
	s.dispatcher = d
	return s
}

// WithSubscriptionInspector wires the Stripe subscription inspector
// used by BulkReconcileBilling.
func (s *PlatformService) WithSubscriptionInspector(i SubscriptionInspector) *PlatformService {
	s.subscriptions = i
	return s
}

// WithClock overrides the time source (used by tests).
func (s *PlatformService) WithClock(fn func() time.Time) *PlatformService {
	if fn != nil {
		s.clock = fn
	}
	return s
}

func (s *PlatformService) now() time.Time { return s.clock().UTC() }

func (s *PlatformService) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// ProvisionWorkspace creates a workspace, its owner admin user, and a
// billing plan row atomically, then best-effort provisions a fabric
// storage tenant and sends a welcome email. The workspace is stamped
// provisioned_by='api'.
//
// tier defaults to the free tier when empty and must otherwise be a
// valid billing tier. placementRef, when non-empty, overrides the
// placement policy recorded on the provisioned storage credentials.
func (s *PlatformService) ProvisionWorkspace(ctx context.Context, name, ownerEmail, tier, placementRef string) (*workspace.Workspace, error) {
	name = strings.TrimSpace(name)
	ownerEmail = strings.TrimSpace(strings.ToLower(ownerEmail))
	tier = strings.TrimSpace(tier)
	if name == "" || ownerEmail == "" {
		return nil, fmt.Errorf("%w: name and owner_email are required", ErrInvalidArgument)
	}
	if tier == "" {
		tier = billing.TierFree
	}
	if !billing.IsValidTier(tier) {
		return nil, fmt.Errorf("%w: unknown tier %q", ErrInvalidArgument, tier)
	}

	tempPassword, err := generateTempPassword()
	if err != nil {
		return nil, err
	}
	ownerName := ownerNameFromEmail(ownerEmail)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("platform: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ws, err := s.workspaces.CreateTx(ctx, tx, name)
	if err != nil {
		return nil, fmt.Errorf("platform: create workspace: %w", err)
	}
	owner, err := s.users.CreateTx(ctx, tx, ws.ID, ownerEmail, ownerName, tempPassword, user.RoleAdmin)
	if err != nil {
		return nil, fmt.Errorf("platform: create owner user: %w", err)
	}
	if err := s.workspaces.SetOwnerTx(ctx, tx, ws.ID, owner.ID); err != nil {
		return nil, fmt.Errorf("platform: set owner: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE workspaces SET tier = $1, provisioned_by = $2, updated_at = now() WHERE id = $3`,
		tier, ProvisionedAPI, ws.ID,
	); err != nil {
		return nil, fmt.Errorf("platform: stamp workspace tier: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO workspace_plans (workspace_id, tier) VALUES ($1, $2)
         ON CONFLICT (workspace_id) DO UPDATE SET tier = EXCLUDED.tier, updated_at = now()`,
		ws.ID, tier,
	); err != nil {
		return nil, fmt.Errorf("platform: create plan: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("platform: commit: %w", err)
	}

	// Best-effort fabric tenant provisioning. A failure must not undo
	// the durable workspace; operators can re-provision storage later.
	if s.provisioner != nil {
		if _, perr := s.provisioner.Provision(ctx, ws.ID, ws.Name); perr != nil {
			if !errors.Is(perr, fabric.ErrConsoleNotConfigured) {
				s.log().Warn("platform: fabric provisioning failed", "workspace_id", ws.ID, "err", perr)
			}
		} else if placementRef = strings.TrimSpace(placementRef); placementRef != "" {
			if _, uerr := s.pool.Exec(ctx,
				`UPDATE workspace_storage_credentials SET placement_policy_ref = $1, updated_at = now() WHERE workspace_id = $2`,
				placementRef, ws.ID,
			); uerr != nil {
				s.log().Warn("platform: placement override failed", "workspace_id", ws.ID, "err", uerr)
			}
		}
	}

	// Best-effort welcome email.
	if s.mailer != nil {
		if merr := s.mailer.SendWorkspaceWelcome(ctx, WelcomeEmail{
			WorkspaceID:   ws.ID,
			WorkspaceName: ws.Name,
			OwnerEmail:    ownerEmail,
			OwnerName:     ownerName,
			TempPassword:  tempPassword,
		}); merr != nil {
			s.log().Warn("platform: welcome email failed", "workspace_id", ws.ID, "err", merr)
		}
	}

	// Re-fetch so the returned struct reflects the stamped tier.
	fresh, err := s.workspaces.GetByID(ctx, ws.ID)
	if err != nil {
		return ws, nil
	}
	return fresh, nil
}

// SuspendWorkspace marks a workspace suspended and revokes every active
// session for its users. Idempotent on the suspended_at timestamp: a
// repeat suspend preserves the original time but refreshes the reason.
func (s *PlatformService) SuspendWorkspace(ctx context.Context, workspaceID uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	tag, err := s.pool.Exec(ctx,
		`UPDATE workspaces
         SET suspended_at = COALESCE(suspended_at, now()),
             suspension_reason = $2,
             updated_at = now()
         WHERE id = $1`,
		workspaceID, nullIfEmpty(reason),
	)
	if err != nil {
		return fmt.Errorf("platform: suspend workspace: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.bustSuspension(ctx, workspaceID)
	s.revokeWorkspaceSessions(ctx, workspaceID)
	return nil
}

// ResumeWorkspace clears the suspension on a workspace.
func (s *PlatformService) ResumeWorkspace(ctx context.Context, workspaceID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE workspaces
         SET suspended_at = NULL, suspension_reason = NULL, updated_at = now()
         WHERE id = $1`,
		workspaceID,
	)
	if err != nil {
		return fmt.Errorf("platform: resume workspace: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.bustSuspension(ctx, workspaceID)
	return nil
}

// revokeWorkspaceSessions force-logs-out every user in the workspace.
// Best-effort: the suspension guard already blocks suspended tenants at
// the middleware, so a transient Redis error here degrades gracefully.
func (s *PlatformService) revokeWorkspaceSessions(ctx context.Context, workspaceID uuid.UUID) {
	if s.sessions == nil {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM users WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		s.log().Warn("platform: list users for revocation failed", "workspace_id", workspaceID, "err", err)
		return
	}
	defer rows.Close()
	var userIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			s.log().Warn("platform: scan user id failed", "workspace_id", workspaceID, "err", err)
			return
		}
		userIDs = append(userIDs, id)
	}
	if err := rows.Err(); err != nil {
		s.log().Warn("platform: iterate users failed", "workspace_id", workspaceID, "err", err)
		return
	}
	now := s.now()
	for _, uid := range userIDs {
		if err := s.sessions.RevokeAllForUser(ctx, workspaceID, uid); err != nil {
			s.log().Warn("platform: revoke sessions failed", "workspace_id", workspaceID, "user_id", uid, "err", err)
		}
		if err := s.sessions.RevokeUser(ctx, workspaceID, uid, now, sessionRevokeTTL); err != nil {
			s.log().Warn("platform: revoke user cutoff failed", "workspace_id", workspaceID, "user_id", uid, "err", err)
		}
	}
}

// WorkspaceSuspension reports whether a workspace is currently
// suspended, along with the reason. Returns (false, "", nil) for an
// unknown workspace so the caller's own not-found handling applies.
// This is the lookup backing the api/middleware suspended-workspace
// 503 guard.
func (s *PlatformService) WorkspaceSuspension(ctx context.Context, workspaceID uuid.UUID) (bool, string, error) {
	if snap, ok := s.cachedSuspension(ctx, workspaceID); ok {
		return snap.Suspended, snap.Reason, nil
	}
	var (
		suspendedAt *time.Time
		reason      *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT suspended_at, suspension_reason FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&suspendedAt, &reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Cache the negative so a flood of requests for an
			// unknown/never-suspended workspace doesn't re-query.
			s.cacheSuspension(ctx, workspaceID, suspensionSnapshot{})
			return false, "", nil
		}
		return false, "", fmt.Errorf("platform: load suspension: %w", err)
	}
	snap := suspensionSnapshot{}
	if suspendedAt != nil {
		snap.Suspended = true
		if reason != nil {
			snap.Reason = *reason
		}
	}
	s.cacheSuspension(ctx, workspaceID, snap)
	return snap.Suspended, snap.Reason, nil
}

// suspensionKey is the Redis key for a workspace's cached suspension
// snapshot.
func (s *PlatformService) suspensionKey(workspaceID uuid.UUID) string {
	return fmt.Sprintf("%s:%s", suspensionCacheKeyPrefix, workspaceID.String())
}

// cachedSuspension returns the workspace's cached suspension snapshot
// when Redis is wired and holds a fresh, parseable entry. A miss, a
// disabled cache, or any Redis/parse error returns ok=false so the
// caller reads through to the authoritative store.
func (s *PlatformService) cachedSuspension(ctx context.Context, workspaceID uuid.UUID) (suspensionSnapshot, bool) {
	if s.rdb == nil {
		return suspensionSnapshot{}, false
	}
	raw, err := s.rdb.Get(ctx, s.suspensionKey(workspaceID)).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logging.FromContext(ctx).Debug("platform: suspension cache read failed, falling through to store",
				"workspace_id", workspaceID, "err", err)
		}
		return suspensionSnapshot{}, false
	}
	var snap suspensionSnapshot
	if jerr := json.Unmarshal(raw, &snap); jerr != nil {
		logging.FromContext(ctx).Warn("platform: discarding unparseable suspension cache entry",
			"workspace_id", workspaceID)
		return suspensionSnapshot{}, false
	}
	return snap, true
}

// cacheSuspension writes a snapshot back to Redis under the short TTL.
// Best-effort: a failed write just means the next read re-queries.
func (s *PlatformService) cacheSuspension(ctx context.Context, workspaceID uuid.UUID, snap suspensionSnapshot) {
	if s.rdb == nil {
		return
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if err := s.rdb.Set(ctx, s.suspensionKey(workspaceID), raw, suspensionCacheTTL).Err(); err != nil {
		logging.FromContext(ctx).Debug("platform: suspension cache write failed",
			"workspace_id", workspaceID, "err", err)
	}
}

// bustSuspension removes the cached snapshot so the next
// WorkspaceSuspension reloads from the store. Called on every
// suspend/resume so the state change takes effect immediately.
// Best-effort: a failed DEL just means the stale entry self-expires
// within suspensionCacheTTL.
func (s *PlatformService) bustSuspension(ctx context.Context, workspaceID uuid.UUID) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.Del(ctx, s.suspensionKey(workspaceID)).Err(); err != nil {
		logging.FromContext(ctx).Debug("platform: suspension cache bust failed",
			"workspace_id", workspaceID, "err", err)
	}
}

// generateTempPassword returns a random URL-safe password used for the
// provisioned owner account. The plaintext is handed to the welcome
// mailer; it is never logged.
func generateTempPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("platform: generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ownerNameFromEmail derives a human-ish display name from the local
// part of the owner email so the owner row is never created with an
// empty name (the users table requires NOT NULL).
func ownerNameFromEmail(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i >= 0 {
		local = email[:i]
	}
	local = strings.TrimSpace(local)
	if local == "" {
		return "Owner"
	}
	return local
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
