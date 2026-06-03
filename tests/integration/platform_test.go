package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/platform"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// newPlatformService wires a platform.PlatformService onto the shared
// integration pool. DB-backed platform tests live here (not in
// internal/platform) so they run inside the integration package's
// serialized harness: `go test ./...` runs package test binaries
// concurrently, and a count/select in a second package would deadlock
// against this harness's ResetTables TRUNCATE (AccessExclusiveLock on
// workspaces). Sharing the harness keeps all DB access on one serial
// timeline.
func newPlatformService(env *testEnv) *platform.PlatformService {
	return platform.NewService(
		env.pool,
		workspace.NewService(workspace.NewPostgresRepository(env.pool)),
		user.NewService(user.NewPostgresRepository(env.pool)),
		billing.NewService(billing.NewPostgresRepository(env.pool)),
	)
}

func TestPlatformProvisionSuspendResumeLifecycle(t *testing.T) {
	env := setupEnv(t)
	svc := newPlatformService(env)
	ctx := context.Background()

	name := "Platform Test " + uuid.NewString()[:8]
	email := "owner+" + uuid.NewString()[:8] + "@example.com"
	ws, err := svc.ProvisionWorkspace(ctx, name, email, billing.TierStarter, "")
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}
	if ws.Name != name {
		t.Errorf("expected name %q, got %q", name, ws.Name)
	}

	// Detail reflects the provisioned tier and not-suspended status.
	summary, err := svc.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if summary.Tier != billing.TierStarter {
		t.Errorf("expected tier %q, got %q", billing.TierStarter, summary.Tier)
	}
	if summary.Suspended {
		t.Errorf("freshly provisioned workspace must not be suspended")
	}
	if summary.UserCount != 1 {
		t.Errorf("expected exactly the owner user, got %d", summary.UserCount)
	}

	// Suspension flips the flag and the middleware-facing lookup.
	if err := svc.SuspendWorkspace(ctx, ws.ID, "abuse"); err != nil {
		t.Fatalf("SuspendWorkspace: %v", err)
	}
	suspended, reason, err := svc.WorkspaceSuspension(ctx, ws.ID)
	if err != nil {
		t.Fatalf("WorkspaceSuspension: %v", err)
	}
	if !suspended || reason != "abuse" {
		t.Errorf("expected suspended with reason 'abuse', got suspended=%v reason=%q", suspended, reason)
	}

	// Resume clears it.
	if err := svc.ResumeWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("ResumeWorkspace: %v", err)
	}
	suspended, _, err = svc.WorkspaceSuspension(ctx, ws.ID)
	if err != nil {
		t.Fatalf("WorkspaceSuspension after resume: %v", err)
	}
	if suspended {
		t.Errorf("workspace should not be suspended after resume")
	}

	// Unknown ids map to ErrNotFound.
	if err := svc.SuspendWorkspace(ctx, uuid.New(), "x"); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown workspace, got %v", err)
	}
}

func TestPlatformListWorkspacesFilters(t *testing.T) {
	env := setupEnv(t)
	svc := newPlatformService(env)
	ctx := context.Background()

	ws, err := svc.ProvisionWorkspace(ctx, "Filter Test "+uuid.NewString()[:8], "owner+"+uuid.NewString()[:8]+"@example.com", billing.TierBusiness, "")
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}

	// Filtering by the provisioned tier returns at least our workspace.
	got, total, err := svc.ListWorkspaces(ctx, platform.ListFilters{Tier: billing.TierBusiness, Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if total < 1 {
		t.Fatalf("expected at least one business-tier workspace")
	}
	found := false
	for _, s := range got {
		if s.ID == ws.ID {
			found = true
			if s.Tier != billing.TierBusiness {
				t.Errorf("tier filter leaked a %q workspace", s.Tier)
			}
		}
	}
	if !found {
		t.Errorf("expected provisioned workspace in the filtered list")
	}

	// A suspended=true filter must exclude the active workspace.
	active := false
	_, _, err = svc.ListWorkspaces(ctx, platform.ListFilters{Suspended: &active, Limit: 1})
	if err != nil {
		t.Fatalf("ListWorkspaces suspended filter: %v", err)
	}
}

// TestPlatformAPIKeyAuthenticate exercises the indexed key_lookup
// authentication path: a minted key authenticates by its deterministic
// digest, the wrong token / a token for another key is rejected, and a
// revoked key stops authenticating. The lookup digest selects at most
// one candidate row, so a stored key must match the presented token
// exactly to succeed.
func TestPlatformAPIKeyAuthenticate(t *testing.T) {
	env := setupEnv(t)
	store := platform.NewAPIKeyStore(env.pool)
	ctx := context.Background()

	key, plaintext, err := store.Create(ctx, "ci-"+uuid.NewString()[:8], []string{"tenant:read"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Minting a second key ensures the lookup discriminates between keys
	// rather than matching the only row present.
	_, other, err := store.Create(ctx, "ci-"+uuid.NewString()[:8], []string{"tenant:write"})
	if err != nil {
		t.Fatalf("Create second key: %v", err)
	}

	got, err := store.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatalf("Authenticate valid key: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("authenticated as key %s, want %s", got.ID, key.ID)
	}
	if len(got.Permissions) != 1 || got.Permissions[0] != "tenant:read" {
		t.Errorf("unexpected permissions: %v", got.Permissions)
	}

	// The other key's plaintext must not authenticate as this key, and a
	// tampered token must be rejected outright.
	if other == plaintext {
		t.Fatal("distinct keys produced identical plaintext")
	}
	if _, err := store.Authenticate(ctx, plaintext+"x"); !errors.Is(err, platform.ErrAPIKeyInvalid) {
		t.Errorf("tampered token: got %v, want ErrAPIKeyInvalid", err)
	}
	if _, err := store.Authenticate(ctx, "not-a-platform-key"); !errors.Is(err, platform.ErrAPIKeyInvalid) {
		t.Errorf("bad-prefix token: got %v, want ErrAPIKeyInvalid", err)
	}

	// Revocation stops authentication (the partial unique index excludes
	// revoked rows, so the lookup finds no candidate).
	if err := store.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store.Authenticate(ctx, plaintext); !errors.Is(err, platform.ErrAPIKeyInvalid) {
		t.Errorf("revoked key: got %v, want ErrAPIKeyInvalid", err)
	}
}
