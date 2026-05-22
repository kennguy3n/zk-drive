// Package tenantctx holds the canonical context key for the
// per-request workspace (tenant) UUID. The key is defined in its own
// package so api/middleware (which attaches the value) and
// internal/database (which reads the value in its pgxpool
// PrepareConn hook to bind the Postgres `app.workspace_id` GUC for
// row-level-security policies) can agree on it without forming an
// import cycle.
//
// Callers OUTSIDE this package should use the
// middleware.WithWorkspaceID / middleware.WorkspaceIDFromContext
// helpers — those are thin wrappers around WithWorkspaceID /
// WorkspaceIDFromContext defined here, and exist so handler code
// doesn't grow a direct dependency on this low-level package.
package tenantctx

import (
	"context"

	"github.com/google/uuid"
)

// contextKey is unexported so values stored under workspaceIDKey
// cannot be retrieved (or overwritten) by code that imports this
// package as a string. context.WithValue's documented contract.
type contextKey int

const workspaceIDKey contextKey = iota

// WithWorkspaceID returns a child context tagged with workspaceID.
// The pgxpool PrepareConn hook in internal/database reads this
// value to set `app.workspace_id` on every connection acquire so
// the row-level-security policies installed by migration
// 024_row_level_security.up.sql can enforce tenant isolation.
//
// An unset workspaceID (uuid.Nil) is treated as "no tenant"; the
// PrepareConn hook will clear `app.workspace_id` on the acquired
// connection so queries fall back to the RLS bypass branch (used
// by login, signup, public share-link resolution, and worker
// jobs that legitimately span workspaces).
func WithWorkspaceID(ctx context.Context, workspaceID uuid.UUID) context.Context {
	return context.WithValue(ctx, workspaceIDKey, workspaceID)
}

// WorkspaceIDFromContext returns the workspace id bound to the
// current context, if any. The boolean is true when the value was
// set AND is non-zero — uuid.Nil sentinels are reported as "not
// set" because they cannot identify a real tenant.
func WorkspaceIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(workspaceIDKey).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return uuid.Nil, false
	}
	return v, true
}
