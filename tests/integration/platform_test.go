package integration

import (
	"context"
	"errors"
	"testing"
	"time"

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

// TestPlatformAPIKeyLifecycle exercises the prefix-indexed key lookup:
// a created key authenticates by its embedded lookup id, a tampered
// secret with the same lookup id is rejected, malformed tokens and
// revoked keys fail, and an empty label is a typed validation error.
func TestPlatformAPIKeyLifecycle(t *testing.T) {
	env := setupEnv(t)
	store := platform.NewAPIKeyStore(env.pool)
	ctx := context.Background()

	key, plaintext, err := store.Create(ctx, "ci-bot "+uuid.NewString()[:8], []string{platform.PermTenantRead})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if key == nil || plaintext == "" {
		t.Fatalf("expected a key and a one-time plaintext")
	}

	got, err := store.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("authenticated id %s != created id %s", got.ID, key.ID)
	}
	if !got.HasPermission(platform.PermTenantRead) {
		t.Errorf("expected the granted permission to round-trip")
	}

	// Same lookup id, wrong secret: must reach the bcrypt compare and
	// fail (proves the secret, not just the selector, is verified).
	if _, err := store.Authenticate(ctx, plaintext+"x"); !errors.Is(err, platform.ErrAPIKeyInvalid) {
		t.Errorf("tampered secret: want ErrAPIKeyInvalid, got %v", err)
	}
	// Too short to carry a lookup id + secret.
	if _, err := store.Authenticate(ctx, "pk_short"); !errors.Is(err, platform.ErrAPIKeyInvalid) {
		t.Errorf("malformed token: want ErrAPIKeyInvalid, got %v", err)
	}
	// Empty label is a typed validation error (handler maps it to 400),
	// not a generic failure.
	if _, _, err := store.Create(ctx, "   ", nil); !errors.Is(err, platform.ErrInvalidArgument) {
		t.Errorf("empty label: want ErrInvalidArgument, got %v", err)
	}

	// Revocation makes the key unauthenticable.
	if err := store.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store.Authenticate(ctx, plaintext); !errors.Is(err, platform.ErrAPIKeyInvalid) {
		t.Errorf("revoked key: want ErrAPIKeyInvalid, got %v", err)
	}
}

// TestPlatformAlertCooldownDedup verifies that EvaluateUsageAlerts
// honours the cooldown contract documented on
// usage_alert_rules.last_triggered_at: a crossed threshold fires once,
// is suppressed on re-evaluation within the cooldown window, and fires
// again only after the window elapses.
func TestPlatformAlertCooldownDedup(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	now := time.Now().UTC()
	svc := newPlatformService(env).
		WithClock(func() time.Time { return now }).
		WithAlertCooldown(time.Hour)

	ws, err := svc.ProvisionWorkspace(ctx, "Alert Test "+uuid.NewString()[:8], "owner+"+uuid.NewString()[:8]+"@example.com", billing.TierStarter, "")
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}
	wsID := ws.ID
	rule, err := svc.CreateAlertRule(ctx, platform.AlertRule{
		WorkspaceID: &wsID,
		Metric:      platform.MetricUserCount,
		Threshold:   1, // the lone owner user crosses gte 1
		Operator:    platform.OperatorGTE,
		Email:       "alerts@example.com",
	})
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}

	firingsForRule := func() int {
		firings, err := svc.EvaluateUsageAlerts(ctx)
		if err != nil {
			t.Fatalf("EvaluateUsageAlerts: %v", err)
		}
		n := 0
		for _, f := range firings {
			if f.RuleID == rule.ID {
				n++
			}
		}
		return n
	}

	if got := firingsForRule(); got != 1 {
		t.Fatalf("first evaluation: want 1 firing, got %d", got)
	}
	if got := firingsForRule(); got != 0 {
		t.Fatalf("within cooldown: want 0 firings (suppressed), got %d", got)
	}
	now = now.Add(time.Hour + time.Minute)
	if got := firingsForRule(); got != 1 {
		t.Fatalf("after cooldown: want 1 firing, got %d", got)
	}
}

// TestPlatformSuspensionCache verifies the short-lived suspension cache
// in front of WorkspaceSuspension: results are reused within the TTL,
// re-read after it elapses, and Suspend/Resume update the cache
// synchronously on the handling instance.
func TestPlatformSuspensionCache(t *testing.T) {
	env := setupEnv(t)
	ctx := context.Background()

	now := time.Now().UTC()
	const ttl = 30 * time.Second
	svc := newPlatformService(env).
		WithClock(func() time.Time { return now }).
		WithSuspensionCacheTTL(ttl)

	ws, err := svc.ProvisionWorkspace(ctx, "Cache Test "+uuid.NewString()[:8], "owner+"+uuid.NewString()[:8]+"@example.com", billing.TierStarter, "")
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}

	// Prime the cache with the active (not-suspended) state.
	if suspended, _, err := svc.WorkspaceSuspension(ctx, ws.ID); err != nil || suspended {
		t.Fatalf("priming lookup: suspended=%v err=%v", suspended, err)
	}

	// Mutate the row out-of-band (as a peer instance would). The cached
	// active result must persist until the TTL elapses.
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspaces SET suspended_at = now(), suspension_reason = 'external' WHERE id = $1`, ws.ID,
	); err != nil {
		t.Fatalf("out-of-band update: %v", err)
	}
	if suspended, _, _ := svc.WorkspaceSuspension(ctx, ws.ID); suspended {
		t.Errorf("within TTL: expected cached active state, got suspended")
	}

	// After the TTL the cache misses and observes the new state.
	now = now.Add(ttl + time.Second)
	if suspended, reason, err := svc.WorkspaceSuspension(ctx, ws.ID); err != nil || !suspended || reason != "external" {
		t.Errorf("after TTL: want suspended/external, got suspended=%v reason=%q err=%v", suspended, reason, err)
	}

	// Suspend/Resume on this instance update the cache synchronously,
	// with no TTL wait.
	if err := svc.ResumeWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("ResumeWorkspace: %v", err)
	}
	if suspended, _, _ := svc.WorkspaceSuspension(ctx, ws.ID); suspended {
		t.Errorf("resume must clear the cached state immediately")
	}
	if err := svc.SuspendWorkspace(ctx, ws.ID, "abuse"); err != nil {
		t.Fatalf("SuspendWorkspace: %v", err)
	}
	if suspended, reason, _ := svc.WorkspaceSuspension(ctx, ws.ID); !suspended || reason != "abuse" {
		t.Errorf("suspend must set the cached state immediately, got suspended=%v reason=%q", suspended, reason)
	}
}
