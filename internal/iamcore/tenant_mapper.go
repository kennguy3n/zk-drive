package iamcore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// ErrNoTenant is returned when an iam-core token carries neither a
// tenant_id nor an org_id claim, so there is no key to resolve a
// workspace from. The middleware maps it to 401 (fail closed): a token
// with no tenant context must not silently land in some default
// workspace.
var ErrNoTenant = errors.New("iamcore: token has no tenant_id or org_id claim")

// TenantMapper resolves an iam-core tenant (the tenant_id + org_id
// claim pair) to a zk-drive workspace, auto-provisioning a workspace
// the first time a tenant is seen. The (tenant, org) -> workspace
// mapping is persisted in iam_core_tenant_workspaces and cached in
// memory so steady-state requests resolve without a database round
// trip.
type TenantMapper struct {
	pool       *pgxpool.Pool
	workspaces *workspace.Service
	cache      sync.Map // mappingKey -> uuid.UUID
}

// NewTenantMapper constructs a TenantMapper backed by the given pool
// and workspace service.
func NewTenantMapper(pool *pgxpool.Pool, workspaces *workspace.Service) *TenantMapper {
	return &TenantMapper{pool: pool, workspaces: workspaces}
}

// mappingKey builds the in-memory cache / lookup key for a tenant.
// Both components are included (NUL-separated, which cannot appear in a
// JWT claim string) so two iam-core orgs under the same tenant — or the
// same org id reused across tenants — never collide onto one workspace.
//
// This key is only ever used as a Go map key. It MUST NOT be sent to
// Postgres as a text bind parameter: PostgreSQL rejects NUL bytes in
// text values (SQLSTATE 22021). The advisory-lock path therefore uses
// advisoryLockKey (a numeric digest computed in Go) instead.
func mappingKey(tenantID, orgID string) string {
	return tenantID + "\x00" + orgID
}

// advisoryLockKey derives the 64-bit key for pg_advisory_xact_lock from
// the (tenantID, orgID) pair. The hash is computed in Go and passed as
// a bigint so no NUL-containing text ever reaches Postgres (which would
// fail with SQLSTATE 22021). A length prefix on the first component
// makes the encoding unambiguous, so ("ab", "") and ("a", "b") map to
// distinct lock keys. Collisions only cause unrelated tenants to
// serialize occasionally, which is harmless for a short provisioning
// critical section.
func advisoryLockKey(tenantID, orgID string) int64 {
	h := fnv.New64a()
	var lenPrefix [8]byte
	binary.BigEndian.PutUint64(lenPrefix[:], uint64(len(tenantID)))
	_, _ = h.Write(lenPrefix[:])
	_, _ = h.Write([]byte(tenantID))
	_, _ = h.Write([]byte(orgID))
	return int64(h.Sum64())
}

// ResolveWorkspace returns the zk-drive workspace id for the given
// iam-core tenant, provisioning one on first sight. displayName is used
// to name a newly provisioned workspace; it is ignored once a mapping
// exists. The context passed here MUST NOT carry a workspace id (it is
// invoked before request context is tenant-bound) so the row-level
// security GUC is unset and the provisioning INSERTs are permitted.
func (m *TenantMapper) ResolveWorkspace(ctx context.Context, tenantID, orgID, displayName string) (uuid.UUID, error) {
	tenantID = strings.TrimSpace(tenantID)
	orgID = strings.TrimSpace(orgID)
	if tenantID == "" && orgID == "" {
		return uuid.Nil, ErrNoTenant
	}
	key := mappingKey(tenantID, orgID)
	if v, ok := m.cache.Load(key); ok {
		return v.(uuid.UUID), nil
	}

	// Fast path: mapping already persisted from an earlier login.
	if id, err := m.lookup(ctx, tenantID, orgID); err == nil {
		m.cache.Store(key, id)
		return id, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	// Slow path: provision atomically under an advisory lock so two
	// concurrent first-logins from the same tenant cannot create two
	// workspaces.
	id, err := m.provision(ctx, tenantID, orgID, displayName)
	if err != nil {
		return uuid.Nil, err
	}
	m.cache.Store(key, id)
	return id, nil
}

func (m *TenantMapper) lookup(ctx context.Context, tenantID, orgID string) (uuid.UUID, error) {
	var id uuid.UUID
	err := m.pool.QueryRow(ctx,
		`SELECT workspace_id FROM iam_core_tenant_workspaces WHERE iam_tenant_id = $1 AND iam_org_id = $2`,
		tenantID, orgID,
	).Scan(&id)
	return id, err
}

// provision creates a workspace and its tenant mapping in a single
// transaction. A transaction-scoped advisory lock keyed on the mapping
// serializes concurrent provisioning for the same tenant; a second
// caller that wins the race re-reads the now-present mapping inside the
// lock and reuses it, so no orphan workspace is ever created.
func (m *TenantMapper) provision(ctx context.Context, tenantID, orgID, displayName string) (uuid.UUID, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("iamcore: begin provisioning tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// pg_advisory_xact_lock's single-argument form takes a bigint, so
	// the lock is released automatically at commit/rollback. The key is
	// hashed in Go (advisoryLockKey) rather than via SQL hashtextextended
	// so the NUL-separated mapping key never has to be sent to Postgres
	// as text (which Postgres rejects, SQLSTATE 22021).
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1)`,
		advisoryLockKey(tenantID, orgID),
	); err != nil {
		return uuid.Nil, fmt.Errorf("iamcore: acquire provisioning lock: %w", err)
	}

	// Re-check inside the lock: another caller may have provisioned
	// between our fast-path miss and acquiring the lock.
	var existing uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT workspace_id FROM iam_core_tenant_workspaces WHERE iam_tenant_id = $1 AND iam_org_id = $2`,
		tenantID, orgID,
	).Scan(&existing)
	switch {
	case err == nil:
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("iamcore: commit provisioning tx: %w", err)
		}
		return existing, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to create
	default:
		return uuid.Nil, fmt.Errorf("iamcore: re-check mapping: %w", err)
	}

	ws, err := m.workspaces.CreateTx(ctx, tx, workspaceName(tenantID, orgID, displayName))
	if err != nil {
		return uuid.Nil, fmt.Errorf("iamcore: create workspace: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO iam_core_tenant_workspaces (iam_tenant_id, iam_org_id, workspace_id)
		 VALUES ($1, $2, $3)`,
		tenantID, orgID, ws.ID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("iamcore: insert tenant mapping: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("iamcore: commit provisioning tx: %w", err)
	}
	return ws.ID, nil
}

// workspaceName derives a human-readable workspace name for a freshly
// provisioned tenant. A provided display name wins; otherwise we fall
// back to whichever tenant identifier is present so an SME admin sees
// something recognizable in the UI rather than a blank name.
func workspaceName(tenantID, orgID, displayName string) string {
	if n := strings.TrimSpace(displayName); n != "" {
		return n
	}
	if orgID != "" {
		return "Organization " + orgID
	}
	return "Tenant " + tenantID
}
