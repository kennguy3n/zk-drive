package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// ErrIPBlocked is returned by IPAllowService.CheckAccess when the
// workspace has IP allowlisting enabled and the client IP is not
// contained by any configured rule. The middleware maps it to a
// 403 with the X-ZkDrive-IP-Blocked header.
var ErrIPBlocked = errors.New("workspace: client IP not in allowlist")

// ErrInvalidCIDR is returned by AddRule when the supplied string is
// not a well-formed CIDR (e.g. "10.0.0.0", "garbage", "1.2.3.4/40").
var ErrInvalidCIDR = errors.New("workspace: invalid CIDR")

// ErrPrivateCIDR is returned by AddRule when the supplied CIDR
// covers a non-public range (RFC1918, loopback, link-local,
// unspecified, multicast). Allowlisting a private range on a public
// multi-tenant SaaS is always a misconfiguration: the gateway never
// observes a private source address, and a NAT'd range would match
// unrelated co-tenants.
var ErrPrivateCIDR = errors.New("workspace: CIDR must be a public range")

// ErrTooManyRules is returned by AddRule when the workspace already
// holds MaxIPRulesPerWorkspace rules.
var ErrTooManyRules = errors.New("workspace: ip allowlist rule cap reached")

// MaxIPRulesPerWorkspace caps the number of allowlist rules a single
// workspace may hold. The cap bounds the cost of CheckAccess (a
// linear scan over the rule set on every request) and the size of
// the cached entry. 50 comfortably covers an SME with several
// offices plus VPN egress ranges.
const MaxIPRulesPerWorkspace = 50

// ipAllowCacheTTL is the lifetime of a cached allowlist snapshot in
// Redis. Short enough that a missed bust (e.g. mutation on another
// replica whose DEL was lost) self-heals within 30s; long enough to
// absorb a request burst without hammering Postgres on the hot
// CheckAccess path.
const ipAllowCacheTTL = 30 * time.Second

// ipAllowCacheKeyPrefix namespaces the per-workspace allowlist cache
// keys. Matches the ws:* convention used by internal/permission and
// internal/session for grep-able Redis keyspaces.
const ipAllowCacheKeyPrefix = "ws:ipallow:v1"

// IPRule is a single allowlist entry: a public CIDR plus audit
// provenance. CIDR is stored and surfaced in canonical text form
// (the net package's IPNet.String()).
type IPRule struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	CIDR        string    `json:"cidr"`
	Label       string    `json:"label"`
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// IPAllowStore is the persistence surface IPAllowService depends on.
// Defined as an interface so the service is unit-testable against an
// in-memory fake (see ipallow_test.go) without a live Postgres
// connection.
type IPAllowStore interface {
	// ListRules returns every rule for the workspace, ordered by
	// created_at ascending.
	ListRules(ctx context.Context, workspaceID uuid.UUID) ([]IPRule, error)
	// CountRules returns the number of rules for the workspace,
	// used to enforce MaxIPRulesPerWorkspace.
	CountRules(ctx context.Context, workspaceID uuid.UUID) (int, error)
	// AddRule inserts rule and returns it with the DB-assigned id
	// and created_at populated.
	AddRule(ctx context.Context, rule IPRule) (IPRule, error)
	// RemoveRule deletes the rule scoped to the workspace. Returns
	// ErrNotFound when no row matches (wrong id or wrong tenant).
	RemoveRule(ctx context.Context, workspaceID, ruleID uuid.UUID) error
	// IsEnabled reports the workspaces.ip_allowlist_enabled flag.
	IsEnabled(ctx context.Context, workspaceID uuid.UUID) (bool, error)
	// SetEnabled flips the flag and returns the previous value so
	// the caller can audit the transition.
	SetEnabled(ctx context.Context, workspaceID uuid.UUID, enabled bool) (previous bool, err error)
}

// IPAllowService implements per-workspace IP allowlisting. Reads on
// the hot CheckAccess path are served from a Redis snapshot
// (enabled flag + the set of CIDRs) with a short TTL; every mutation
// busts the snapshot. Redis is a pure accelerator — any Redis error
// falls through to the authoritative Postgres store, so a Redis
// outage degrades latency, never correctness.
type IPAllowService struct {
	store IPAllowStore
	rdb   redis.UniversalClient
	ttl   time.Duration
}

// ipAllowSnapshot is the cached representation of a workspace's
// allowlist state. CIDRs is the canonical text of every rule; the
// service re-parses them on read rather than caching net.IPNet
// values (which don't round-trip through JSON cleanly).
type ipAllowSnapshot struct {
	Enabled bool     `json:"enabled"`
	CIDRs   []string `json:"cidrs"`
}

// NewIPAllowService constructs a service backed by store. rdb may be
// nil (caching disabled — every CheckAccess reads through to the
// store); ttl <= 0 falls back to ipAllowCacheTTL.
func NewIPAllowService(store IPAllowStore, rdb redis.UniversalClient) *IPAllowService {
	return &IPAllowService{store: store, rdb: rdb, ttl: ipAllowCacheTTL}
}

// CheckAccess returns nil when clientIP is permitted for the
// workspace and ErrIPBlocked when it is not. When allowlisting is
// disabled for the workspace it always returns nil without
// consulting the rule set. A nil clientIP with allowlisting enabled
// is treated as a block (the request's source could not be
// resolved, so it cannot be proven to be on an allowed network).
func (s *IPAllowService) CheckAccess(ctx context.Context, workspaceID uuid.UUID, clientIP net.IP) error {
	snap, err := s.snapshot(ctx, workspaceID)
	if err != nil {
		return err
	}
	if !snap.Enabled {
		return nil
	}
	if clientIP == nil {
		return ErrIPBlocked
	}
	for _, c := range snap.CIDRs {
		_, network, perr := net.ParseCIDR(c)
		if perr != nil {
			// A malformed CIDR should never reach storage
			// (AddRule validates), but if one does we skip it
			// rather than letting it fail the whole check open
			// or closed for unrelated rules.
			logging.FromContext(ctx).Warn("ip allowlist: skipping unparseable stored CIDR",
				"workspace_id", workspaceID, "cidr", c, "err", perr)
			continue
		}
		if network.Contains(clientIP) {
			return nil
		}
	}
	return ErrIPBlocked
}

// snapshot returns the workspace's allowlist state, served from the
// Redis cache when present and fresh, otherwise loaded from the
// store and written back. Redis errors degrade to a direct store
// read.
func (s *IPAllowService) snapshot(ctx context.Context, workspaceID uuid.UUID) (ipAllowSnapshot, error) {
	if s.rdb != nil {
		if raw, err := s.rdb.Get(ctx, s.cacheKey(workspaceID)).Bytes(); err == nil {
			var snap ipAllowSnapshot
			if jerr := json.Unmarshal(raw, &snap); jerr == nil {
				return snap, nil
			}
			// Corrupt cache entry: fall through to the store.
			logging.FromContext(ctx).Warn("ip allowlist: discarding unparseable cache entry",
				"workspace_id", workspaceID)
		} else if !errors.Is(err, redis.Nil) {
			logging.FromContext(ctx).Debug("ip allowlist: redis read failed, falling through to store",
				"workspace_id", workspaceID, "err", err)
		}
	}

	snap, err := s.loadFromStore(ctx, workspaceID)
	if err != nil {
		return ipAllowSnapshot{}, err
	}
	s.writeCache(ctx, workspaceID, snap)
	return snap, nil
}

func (s *IPAllowService) loadFromStore(ctx context.Context, workspaceID uuid.UUID) (ipAllowSnapshot, error) {
	enabled, err := s.store.IsEnabled(ctx, workspaceID)
	if err != nil {
		return ipAllowSnapshot{}, fmt.Errorf("read ip_allowlist_enabled: %w", err)
	}
	snap := ipAllowSnapshot{Enabled: enabled}
	// Skip the rule fetch entirely when disabled — the common case
	// for the vast majority of workspaces, and CheckAccess never
	// looks at CIDRs when disabled anyway.
	if !enabled {
		return snap, nil
	}
	rules, err := s.store.ListRules(ctx, workspaceID)
	if err != nil {
		return ipAllowSnapshot{}, fmt.Errorf("list ip rules: %w", err)
	}
	snap.CIDRs = make([]string, 0, len(rules))
	for _, r := range rules {
		snap.CIDRs = append(snap.CIDRs, r.CIDR)
	}
	return snap, nil
}

func (s *IPAllowService) writeCache(ctx context.Context, workspaceID uuid.UUID, snap ipAllowSnapshot) {
	if s.rdb == nil {
		return
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return
	}
	ttl := s.ttl
	if ttl <= 0 {
		ttl = ipAllowCacheTTL
	}
	if err := s.rdb.Set(ctx, s.cacheKey(workspaceID), raw, ttl).Err(); err != nil {
		logging.FromContext(ctx).Debug("ip allowlist: redis write failed",
			"workspace_id", workspaceID, "err", err)
	}
}

// bust removes the cached snapshot so the next CheckAccess reloads
// from the store. Best-effort: a failed DEL just means the stale
// entry self-expires within the TTL.
func (s *IPAllowService) bust(ctx context.Context, workspaceID uuid.UUID) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.Del(ctx, s.cacheKey(workspaceID)).Err(); err != nil {
		logging.FromContext(ctx).Debug("ip allowlist: cache bust failed",
			"workspace_id", workspaceID, "err", err)
	}
}

func (s *IPAllowService) cacheKey(workspaceID uuid.UUID) string {
	return fmt.Sprintf("%s:%s", ipAllowCacheKeyPrefix, workspaceID.String())
}

// ListRules returns every allowlist rule for the workspace.
func (s *IPAllowService) ListRules(ctx context.Context, workspaceID uuid.UUID) ([]IPRule, error) {
	return s.store.ListRules(ctx, workspaceID)
}

// AddRule validates cidr (well-formed and public), enforces the
// per-workspace rule cap, persists the rule, and busts the cache.
// The returned rule carries the DB-assigned id and created_at.
func (s *IPAllowService) AddRule(ctx context.Context, workspaceID uuid.UUID, cidr, label string, createdBy uuid.UUID) (*IPRule, error) {
	canonical, err := ValidatePublicCIDR(cidr)
	if err != nil {
		return nil, err
	}
	count, err := s.store.CountRules(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("count ip rules: %w", err)
	}
	if count >= MaxIPRulesPerWorkspace {
		return nil, ErrTooManyRules
	}
	saved, err := s.store.AddRule(ctx, IPRule{
		WorkspaceID: workspaceID,
		CIDR:        canonical,
		Label:       label,
		CreatedBy:   createdBy,
	})
	if err != nil {
		return nil, fmt.Errorf("add ip rule: %w", err)
	}
	s.bust(ctx, workspaceID)
	return &saved, nil
}

// RemoveRule deletes a rule scoped to the workspace and busts the
// cache. Returns ErrNotFound when the rule does not exist for the
// workspace.
func (s *IPAllowService) RemoveRule(ctx context.Context, workspaceID, ruleID uuid.UUID) error {
	if err := s.store.RemoveRule(ctx, workspaceID, ruleID); err != nil {
		return err
	}
	s.bust(ctx, workspaceID)
	return nil
}

// SetEnabled flips the workspace's ip_allowlist_enabled flag and
// busts the cache. Returns the previous value so the caller can
// record the transition in the audit log.
func (s *IPAllowService) SetEnabled(ctx context.Context, workspaceID uuid.UUID, enabled bool) (bool, error) {
	prev, err := s.store.SetEnabled(ctx, workspaceID, enabled)
	if err != nil {
		return false, err
	}
	s.bust(ctx, workspaceID)
	return prev, nil
}

// IsEnabled reports whether allowlisting is active for the
// workspace. Reads through to the store (not the cache) so admin
// UIs always render the authoritative flag.
func (s *IPAllowService) IsEnabled(ctx context.Context, workspaceID uuid.UUID) (bool, error) {
	return s.store.IsEnabled(ctx, workspaceID)
}

// ValidatePublicCIDR parses cidr and confirms it is a well-formed,
// public network. Returns the canonical text form (host bits
// zeroed, e.g. "203.0.113.5/24" -> "203.0.113.0/24"). Returns
// ErrInvalidCIDR for malformed input and ErrPrivateCIDR for
// non-public ranges (RFC1918, loopback, link-local, unspecified,
// multicast). Exported so handlers and tests share one definition.
func ValidatePublicCIDR(cidr string) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", ErrInvalidCIDR
	}
	if !isPublicIP(network.IP) {
		return "", ErrPrivateCIDR
	}
	return network.String(), nil
}

// isPublicIP reports whether ip is routable on the public internet
// for allowlisting purposes. Anything an internet-facing gateway
// would never legitimately see as a source — private, loopback,
// link-local, unspecified, or multicast — is rejected.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsPrivate() {
		return false
	}
	return true
}

// PostgresIPAllowStore implements IPAllowStore against Postgres.
type PostgresIPAllowStore struct {
	pool *pgxpool.Pool
}

// NewPostgresIPAllowStore returns a store using the supplied pool.
func NewPostgresIPAllowStore(pool *pgxpool.Pool) *PostgresIPAllowStore {
	return &PostgresIPAllowStore{pool: pool}
}

// ListRules returns every rule for the workspace ordered by
// created_at. cidr is cast to text on read so it scans cleanly into
// a Go string regardless of the pgx type map's CIDR handling.
func (r *PostgresIPAllowStore) ListRules(ctx context.Context, workspaceID uuid.UUID) ([]IPRule, error) {
	const q = `
SELECT id, workspace_id, cidr::text, label, created_by, created_at
FROM workspace_ip_allowlist
WHERE workspace_id = $1
ORDER BY created_at ASC`
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("query ip rules: %w", err)
	}
	defer rows.Close()

	var out []IPRule
	for rows.Next() {
		var rule IPRule
		if err := rows.Scan(&rule.ID, &rule.WorkspaceID, &rule.CIDR, &rule.Label, &rule.CreatedBy, &rule.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan ip rule: %w", err)
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ip rules: %w", err)
	}
	return out, nil
}

// CountRules returns the number of rules for the workspace.
func (r *PostgresIPAllowStore) CountRules(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	const q = `SELECT count(*) FROM workspace_ip_allowlist WHERE workspace_id = $1`
	var n int
	if err := r.pool.QueryRow(ctx, q, workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count ip rules: %w", err)
	}
	return n, nil
}

// AddRule inserts a rule. The id is generated in Go (mirroring
// insertWorkspace) so the value is known without a RETURNING round
// trip on every column; created_at is returned from the DB default.
// The CIDR is bound as text and cast to the cidr column type.
func (r *PostgresIPAllowStore) AddRule(ctx context.Context, rule IPRule) (IPRule, error) {
	if rule.ID == uuid.Nil {
		rule.ID = uuid.New()
	}
	const q = `
INSERT INTO workspace_ip_allowlist (id, workspace_id, cidr, label, created_by)
VALUES ($1, $2, $3::cidr, $4, $5)
RETURNING created_at`
	if err := r.pool.QueryRow(ctx, q, rule.ID, rule.WorkspaceID, rule.CIDR, rule.Label, rule.CreatedBy).
		Scan(&rule.CreatedAt); err != nil {
		return IPRule{}, fmt.Errorf("insert ip rule: %w", err)
	}
	return rule, nil
}

// RemoveRule deletes a rule scoped to the workspace. The workspace_id
// predicate is defence in depth on top of RLS so a rule id from one
// tenant can never delete another tenant's row even on the bypass
// connection path.
func (r *PostgresIPAllowStore) RemoveRule(ctx context.Context, workspaceID, ruleID uuid.UUID) error {
	const q = `DELETE FROM workspace_ip_allowlist WHERE id = $1 AND workspace_id = $2`
	tag, err := r.pool.Exec(ctx, q, ruleID, workspaceID)
	if err != nil {
		return fmt.Errorf("delete ip rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IsEnabled reads workspaces.ip_allowlist_enabled.
func (r *PostgresIPAllowStore) IsEnabled(ctx context.Context, workspaceID uuid.UUID) (bool, error) {
	const q = `SELECT ip_allowlist_enabled FROM workspaces WHERE id = $1`
	var enabled bool
	if err := r.pool.QueryRow(ctx, q, workspaceID).Scan(&enabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("read ip_allowlist_enabled: %w", err)
	}
	return enabled, nil
}

// SetEnabled flips the flag under a SELECT ... FOR UPDATE / UPDATE
// pair so a concurrent toggle can't misreport the previous value to
// the audit log. Same concurrency reasoning as SetMFARequired.
func (r *PostgresIPAllowStore) SetEnabled(ctx context.Context, workspaceID uuid.UUID, enabled bool) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prev bool
	if err := tx.QueryRow(ctx,
		"SELECT ip_allowlist_enabled FROM workspaces WHERE id = $1 FOR UPDATE",
		workspaceID,
	).Scan(&prev); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("read ip_allowlist_enabled: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"UPDATE workspaces SET ip_allowlist_enabled = $2, updated_at = now() WHERE id = $1",
		workspaceID, enabled,
	); err != nil {
		return false, fmt.Errorf("update ip_allowlist_enabled: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit ip_allowlist_enabled: %w", err)
	}
	return prev, nil
}
