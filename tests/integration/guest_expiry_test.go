package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/sharing"
)

// TestGuestExpirySweep verifies ExpireGuestAccess deletes guest
// permission rows whose expires_at is in the past. The test creates a
// guest invite, mutates its matching permission's expires_at into the
// past, runs the sweep, and confirms the permission row is gone.
func TestGuestExpirySweep(t *testing.T) {
	env := setupEnv(t)
	admin := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	f := createFolder(t, env, admin.Token, nil, "Shared")

	// Create an invite that is live right now.
	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	status, body := env.httpRequest(http.MethodPost, "/api/guest-invites", admin.Token, createGuestInvitePayload{
		Email:     "guest@example.test",
		FolderID:  f.ID.String(),
		Role:      "viewer",
		ExpiresAt: &future,
	})
	if status != http.StatusCreated {
		t.Fatalf("create invite: status=%d body=%s", status, string(body))
	}
	var inv sharing.GuestInvite
	env.decodeJSON(body, &inv)

	// Back-date the matching permission row so the sweep considers
	// it expired. We update the permission directly because the
	// service's expires_at validation rejects past timestamps.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	past := time.Now().UTC().Add(-1 * time.Hour)
	if _, err := env.pool.Exec(ctx,
		`UPDATE permissions SET expires_at = $2 WHERE id = $1`,
		inv.PermissionID, past,
	); err != nil {
		t.Fatalf("back-date permission: %v", err)
	}

	sharingSvc := sharing.NewService(
		sharing.NewPostgresRepository(env.pool),
		testPermissionGranter{svc: permission.NewService(permission.NewPostgresRepository(env.pool))},
	)
	revoked, err := sharingSvc.ExpireGuestAccess(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if revoked != 1 {
		t.Errorf("expected 1 permission revoked, got %d", revoked)
	}

	var exists bool
	if err := env.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM permissions WHERE id = $1)`, inv.PermissionID,
	).Scan(&exists); err != nil {
		t.Fatalf("check permission: %v", err)
	}
	if exists {
		t.Error("permission row still present after sweep")
	}

	// Second sweep is a no-op.
	revoked, err = sharingSvc.ExpireGuestAccess(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if revoked != 0 {
		t.Errorf("expected 0 permissions on second sweep, got %d", revoked)
	}

	// Live (non-expired) invites should not be touched. Re-create
	// one and confirm the sweep leaves it alone.
	status, body = env.httpRequest(http.MethodPost, "/api/guest-invites", admin.Token, createGuestInvitePayload{
		Email:     "live@example.test",
		FolderID:  f.ID.String(),
		Role:      "viewer",
		ExpiresAt: &future,
	})
	if status != http.StatusCreated {
		t.Fatalf("recreate invite: status=%d body=%s", status, string(body))
	}
	var live sharing.GuestInvite
	env.decodeJSON(body, &live)
	if _, err := sharingSvc.ExpireGuestAccess(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	var livePermID uuid.UUID
	if err := env.pool.QueryRow(ctx,
		`SELECT id FROM permissions WHERE id = $1`, live.PermissionID,
	).Scan(&livePermID); err != nil {
		t.Fatalf("live permission missing after sweep: %v", err)
	}
}
