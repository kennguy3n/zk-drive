package integration

import (
	"context"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/tenantctx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestDeviceTokenRepositoryCRUD exercises the migration 039 table and the
// PostgresRepository device-token methods against a real database:
// upsert idempotency, listing, and idempotent delete. The test connection
// is a superuser (BYPASSRLS), so this validates the SQL contract;
// tenant isolation is covered separately by TestDeviceTokenRLSIsolation
// under a non-privileged role where the policy actually fires.
func TestDeviceTokenRepositoryCRUD(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("DeviceCo", "admin@device.test", "Dana", "password-dd")
	workspaceID := uuid.MustParse(tok.WorkspaceID)
	userID := uuid.MustParse(tok.UserID)

	repo := notification.NewPostgresRepository(env.pool)
	ctx := tenantctx.WithWorkspaceID(context.Background(), workspaceID)

	ios := notification.DeviceToken{Platform: notification.PlatformIOS, Token: "apns-token-1"}
	android := notification.DeviceToken{Platform: notification.PlatformAndroid, Token: "fcm-token-1"}

	if err := repo.SaveDeviceToken(ctx, workspaceID, userID, ios); err != nil {
		t.Fatalf("save ios: %v", err)
	}
	if err := repo.SaveDeviceToken(ctx, workspaceID, userID, android); err != nil {
		t.Fatalf("save android: %v", err)
	}
	// Re-saving the same token must be an upsert, not a duplicate row.
	if err := repo.SaveDeviceToken(ctx, workspaceID, userID, ios); err != nil {
		t.Fatalf("re-save ios: %v", err)
	}

	tokens, err := repo.ListDeviceTokens(ctx, workspaceID, userID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens after upsert, got %d: %+v", len(tokens), tokens)
	}

	// Idempotent delete: removing a present and an absent token both succeed.
	if err := repo.DeleteDeviceToken(ctx, workspaceID, userID, ios); err != nil {
		t.Fatalf("delete ios: %v", err)
	}
	if err := repo.DeleteDeviceToken(ctx, workspaceID, userID, ios); err != nil {
		t.Fatalf("delete absent ios: %v", err)
	}
	tokens, err = repo.ListDeviceTokens(ctx, workspaceID, userID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Token != "fcm-token-1" {
		t.Fatalf("expected only the android token to remain, got %+v", tokens)
	}
}

// TestDeviceTokenRLSIsolation verifies a device token written under one
// workspace is invisible to another, enforced by the tenant_isolation
// row-level-security policy from migration 039. The superuser test
// connection bypasses RLS, so the read side runs under the
// non-privileged rls_test_role (via runAsRLSRole) where the policy
// actually fires — mirroring how request-scoped queries run in
// production.
func TestDeviceTokenRLSIsolation(t *testing.T) {
	env := setupEnv(t)
	ensureRLSTestRole(t, env)
	tokA := env.signupAndLogin("WorkspaceA", "alice@device.test", "Alice", "password-aa")
	tokB := env.signupAndLogin("WorkspaceB", "bob@device.test", "Bob", "password-bb")
	wsA := uuid.MustParse(tokA.WorkspaceID)
	userA := uuid.MustParse(tokA.UserID)
	wsB := uuid.MustParse(tokB.WorkspaceID)

	// Commit a token for workspace A through the repository (superuser
	// connection, so the write itself is not gated by RLS).
	repo := notification.NewPostgresRepository(env.pool)
	ctxA := tenantctx.WithWorkspaceID(context.Background(), wsA)
	if err := repo.SaveDeviceToken(ctxA, wsA, userA, notification.DeviceToken{Platform: notification.PlatformIOS, Token: "a-secret-token"}); err != nil {
		t.Fatalf("save under workspace A: %v", err)
	}

	countFor := func(wsID uuid.UUID) int {
		var n int
		runAsRLSRole(t, env, wsID, func(t *testing.T, tx pgx.Tx) {
			if err := tx.QueryRow(context.Background(),
				`SELECT count(*) FROM device_push_tokens WHERE token = $1`, "a-secret-token").Scan(&n); err != nil {
				t.Fatalf("count under workspace %s: %v", wsID, err)
			}
		})
		return n
	}

	if got := countFor(wsA); got != 1 {
		t.Fatalf("workspace A should see its own token, saw %d", got)
	}
	if got := countFor(wsB); got != 0 {
		t.Fatalf("RLS breach: workspace B saw %d of workspace A's tokens", got)
	}
}
