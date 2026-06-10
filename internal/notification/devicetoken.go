package notification

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Platform identifies the native push transport a device token belongs
// to. It is persisted verbatim in device_push_tokens.platform (migration
// 039), whose CHECK constraint pins the column to exactly these two
// values, and it selects the delivery provider in the MobilePushService
// fan-out.
type Platform string

const (
	// PlatformIOS routes through APNs (Apple Push Notification service).
	PlatformIOS Platform = "ios"
	// PlatformAndroid routes through FCM (Firebase Cloud Messaging).
	PlatformAndroid Platform = "android"
)

// Valid reports whether p is one of the recognised platforms. Used by
// the register handler to reject an unknown platform with a 400 before
// it reaches the database CHECK constraint.
func (p Platform) Valid() bool {
	return p == PlatformIOS || p == PlatformAndroid
}

// DeviceToken is a single native push registration: the opaque token a
// platform push service (APNs / FCM) issued for one app install, plus
// the platform that selects its delivery provider. It is the mobile
// analogue of PushSubscription (browser Web Push).
//
// The token is an opaque routing handle — not secret key material — so
// it is stored as plaintext, exactly like PushSubscription.Endpoint
// (and unlike the p256dh / auth key material, which the repository
// encrypts at rest).
type DeviceToken struct {
	Platform Platform `json:"platform"`
	Token    string   `json:"token"`
}

// DeviceTokenRepository persists native mobile push device tokens. The
// Postgres implementation is *PostgresRepository (methods below), reused
// from the same pool that backs notifications and Web Push.
type DeviceTokenRepository interface {
	// SaveDeviceToken upserts a token for (workspace, user, platform,
	// token). Re-registering the same token refreshes updated_at rather
	// than creating a duplicate row.
	SaveDeviceToken(ctx context.Context, workspaceID, userID uuid.UUID, dt DeviceToken) error
	// DeleteDeviceToken removes a single token. Missing rows are not an
	// error (idempotent unregister / dead-token prune).
	DeleteDeviceToken(ctx context.Context, workspaceID, userID uuid.UUID, dt DeviceToken) error
	// ListDeviceTokens returns every device token registered for
	// (workspace, user) — a user signed in on multiple phones has
	// multiple rows.
	ListDeviceTokens(ctx context.Context, workspaceID, userID uuid.UUID) ([]DeviceToken, error)
}

// SaveDeviceToken upserts a native push device token. The
// UNIQUE(workspace_id, user_id, platform, token) constraint
// (migration 039) means re-registering the same token refreshes its
// updated_at instead of inserting a duplicate, so an install that
// re-registers on every cold start keeps one fresh row.
func (r *PostgresRepository) SaveDeviceToken(ctx context.Context, workspaceID, userID uuid.UUID, dt DeviceToken) error {
	const q = `
INSERT INTO device_push_tokens (workspace_id, user_id, platform, token)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, user_id, platform, token)
DO UPDATE SET updated_at = now()
`
	if _, err := r.pool.Exec(ctx, q, workspaceID, userID, string(dt.Platform), dt.Token); err != nil {
		return fmt.Errorf("save device token: %w", err)
	}
	return nil
}

// DeleteDeviceToken removes a single device token. Idempotent: deleting
// an unknown token still returns nil so sign-out and dead-token pruning
// never fail on an already-absent row.
func (r *PostgresRepository) DeleteDeviceToken(ctx context.Context, workspaceID, userID uuid.UUID, dt DeviceToken) error {
	if _, err := r.pool.Exec(ctx,
		`DELETE FROM device_push_tokens
         WHERE workspace_id = $1 AND user_id = $2 AND platform = $3 AND token = $4`,
		workspaceID, userID, string(dt.Platform), dt.Token); err != nil {
		return fmt.Errorf("delete device token: %w", err)
	}
	return nil
}

// ListDeviceTokens returns every device token for (workspace, user).
// Used by the MobilePushPublisher to fan a notification out to all of a
// user's registered phones.
func (r *PostgresRepository) ListDeviceTokens(ctx context.Context, workspaceID, userID uuid.UUID) ([]DeviceToken, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT platform, token FROM device_push_tokens
         WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("list device tokens: %w", err)
	}
	defer rows.Close()
	var out []DeviceToken
	for rows.Next() {
		var dt DeviceToken
		var platform string
		if err := rows.Scan(&platform, &dt.Token); err != nil {
			return nil, err
		}
		dt.Platform = Platform(platform)
		out = append(out, dt)
	}
	return out, rows.Err()
}
