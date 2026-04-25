// Package wiring holds small adapters that bridge service interfaces
// across packages without forcing those packages to import each
// other. Both cmd/server and cmd/worker need the same adapter that
// turns a *permission.Service into a sharing.PermissionGranter, so
// the implementation lives here instead of being duplicated in each
// binary's main.go.
package wiring

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/sharing"
)

// PermissionGranterAdapter bridges *permission.Service to
// sharing.PermissionGranter. The sharing package can't import
// permission directly without creating a dependency loop in future
// packages that want to use both sides, so this adapter sits in its
// own package where the full dependency graph is visible.
type PermissionGranterAdapter struct {
	Service *permission.Service
}

// NewPermissionGranter returns an adapter wrapping svc.
func NewPermissionGranter(svc *permission.Service) PermissionGranterAdapter {
	return PermissionGranterAdapter{Service: svc}
}

// Grant proxies to permission.Service.Grant and converts the returned
// permission row into a sharing.PermissionRef.
func (a PermissionGranterAdapter) Grant(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, role string, expiresAt *time.Time) (sharing.PermissionRef, error) {
	p, err := a.Service.Grant(ctx, workspaceID, resourceType, resourceID, granteeType, granteeID, role, expiresAt)
	if err != nil {
		return sharing.PermissionRef{}, err
	}
	return sharing.PermissionRef{ID: p.ID}, nil
}

// Revoke proxies to permission.Service.Revoke.
func (a PermissionGranterAdapter) Revoke(ctx context.Context, workspaceID, permID uuid.UUID) error {
	return a.Service.Revoke(ctx, workspaceID, permID)
}
