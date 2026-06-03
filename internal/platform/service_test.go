package platform

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/user"
	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// --- pure-logic unit tests (no database) -----------------------------

func TestThresholdCrossed(t *testing.T) {
	cases := []struct {
		op        string
		value     float64
		threshold float64
		want      bool
	}{
		{OperatorGTE, 90, 80, true},
		{OperatorGTE, 80, 80, true},
		{OperatorGTE, 79.9, 80, false},
		{OperatorLTE, 10, 20, true},
		{OperatorLTE, 20, 20, true},
		{OperatorLTE, 21, 20, false},
		{"unknown", 100, 0, false}, // fail safe: never fire on bad operator
		{"", 100, 0, false},
	}
	for _, c := range cases {
		if got := thresholdCrossed(c.op, c.value, c.threshold); got != c.want {
			t.Errorf("thresholdCrossed(%q, %v, %v) = %v, want %v", c.op, c.value, c.threshold, got, c.want)
		}
	}
}

func TestValidMetricAndOperator(t *testing.T) {
	for _, m := range []string{MetricStoragePercent, MetricUserCount, MetricBandwidthMonthlyGB} {
		if !validMetric(m) {
			t.Errorf("expected %q to be a valid metric", m)
		}
	}
	for _, m := range []string{"", "cpu", "Storage_Percent"} {
		if validMetric(m) {
			t.Errorf("expected %q to be invalid", m)
		}
	}
	if !validOperator(OperatorGTE) || !validOperator(OperatorLTE) {
		t.Errorf("gte/lte must be valid operators")
	}
	if validOperator("eq") || validOperator("") {
		t.Errorf("unexpected operator accepted")
	}
}

func TestStoragePercent(t *testing.T) {
	cases := []struct {
		used, quota int64
		want        float64
	}{
		{0, 0, 0},     // unlimited / unset quota -> 0
		{100, 0, 0},   // divide-by-zero guard
		{50, 100, 50}, // half full
		{100, 100, 100},
		{150, 100, 150}, // over quota reports >100
	}
	for _, c := range cases {
		if got := storagePercent(c.used, c.quota); got != c.want {
			t.Errorf("storagePercent(%d, %d) = %v, want %v", c.used, c.quota, got, c.want)
		}
	}
}

func TestOwnerNameFromEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "alice",
		"bob.smith@corp.io": "bob.smith",
		"@nolocal.com":      "Owner", // empty local part falls back
		"noatsign":          "noatsign",
		"":                  "Owner",
	}
	for in, want := range cases {
		if got := ownerNameFromEmail(in); got != want {
			t.Errorf("ownerNameFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNullIfEmpty(t *testing.T) {
	if nullIfEmpty("") != nil {
		t.Errorf("empty string must map to nil")
	}
	if got := nullIfEmpty("x"); got == nil || *got != "x" {
		t.Errorf("non-empty string must map to its pointer")
	}
}

// --- API key generation / verification ------------------------------

func TestGenerateAndHashAPIKey(t *testing.T) {
	key, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey: %v", err)
	}
	if !strings.HasPrefix(key, APIKeyPrefix) {
		t.Fatalf("key %q missing prefix %q", key, APIKeyPrefix)
	}
	// Two generations must differ.
	key2, _ := generateAPIKey()
	if key == key2 {
		t.Fatalf("expected distinct keys, got identical")
	}

	hash, err := hashAPIKey(key)
	if err != nil {
		t.Fatalf("hashAPIKey: %v", err)
	}
	// bcrypt verifies the original and rejects a different key.
	if err := bcrypt.CompareHashAndPassword(hash, []byte(key)); err != nil {
		t.Errorf("expected hash to verify original key: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(key2)); err == nil {
		t.Errorf("expected hash to reject a different key")
	}
}

func TestAPIKeyHasPermission(t *testing.T) {
	k := &APIKey{Permissions: []string{PermTenantRead, PermTenantSuspend}}
	if !k.HasPermission(PermTenantRead) || !k.HasPermission(PermTenantSuspend) {
		t.Errorf("expected granted permissions to be reported")
	}
	if k.HasPermission(PermTenantWrite) {
		t.Errorf("permissions must be explicit, not implied")
	}
	var nilKey *APIKey
	if nilKey.HasPermission(PermTenantRead) {
		t.Errorf("nil key must never grant a permission")
	}
}

// --- reconcileOne (no DB; uses a fake inspector) ---------------------

type fakeInspector struct {
	status string
	tier   string
	err    error
}

func (f fakeInspector) SubscriptionStatus(_ context.Context, _ string) (string, string, error) {
	return f.status, f.tier, f.err
}

func strptr(s string) *string { return &s }

func TestReconcileOneWithoutInspector(t *testing.T) {
	s := NewService(nil, nil, nil, nil)
	id := uuid.New()

	// Free tier without a customer is consistent.
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierFree, nil); mismatch {
		t.Errorf("free tier without customer should not be a mismatch")
	}
	// Paid tier without a customer is a mismatch.
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, nil); !mismatch {
		t.Errorf("paid tier without customer should be a mismatch")
	}
	// Any linked customer is unverifiable without an inspector.
	if entry, mismatch := s.reconcileOne(context.Background(), id, billing.TierFree, strptr("cus_1")); !mismatch || !strings.Contains(entry.Reason, "unverified") {
		t.Errorf("linked customer without inspector should be flagged unverified, got mismatch=%v reason=%q", mismatch, entry.Reason)
	}
}

func TestReconcileOneWithInspector(t *testing.T) {
	id := uuid.New()

	// Matching tier and active subscription -> no mismatch.
	s := NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{status: "active", tier: billing.TierBusiness})
	if entry, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, strptr("cus_ok")); mismatch {
		t.Errorf("matching tier should reconcile cleanly, got reason %q", entry.Reason)
	}

	// Tier drift -> mismatch.
	s = NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{status: "active", tier: billing.TierStarter})
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, strptr("cus_drift")); !mismatch {
		t.Errorf("tier drift should be a mismatch")
	}

	// No subscription found for a linked customer -> mismatch.
	s = NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{status: ""})
	if _, mismatch := s.reconcileOne(context.Background(), id, billing.TierBusiness, strptr("cus_none")); !mismatch {
		t.Errorf("missing subscription should be a mismatch")
	}

	// Inspector error -> mismatch with reason.
	s = NewService(nil, nil, nil, nil).WithSubscriptionInspector(fakeInspector{err: errors.New("boom")})
	if entry, mismatch := s.reconcileOne(context.Background(), id, billing.TierFree, strptr("cus_err")); !mismatch || !strings.Contains(entry.Reason, "boom") {
		t.Errorf("inspector error should surface as a mismatch, got mismatch=%v reason=%q", mismatch, entry.Reason)
	}
}

// --- DB-backed lifecycle test (skips when no database) ---------------

// newDBService dials TEST_DATABASE_URL and returns a fully-wired
// service. The test is skipped (not failed) when the database is
// unreachable or the platform schema (migrations 036/037) is absent,
// so the package's unit tests still run in environments without a DB.
func newDBService(t *testing.T) (*PlatformService, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-backed platform test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("cannot connect to TEST_DATABASE_URL: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot ping TEST_DATABASE_URL: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='workspaces' AND column_name='suspended_at')`,
	).Scan(&exists); err != nil || !exists {
		pool.Close()
		t.Skip("platform migrations (036/037) not applied; skipping DB-backed platform test")
	}
	svc := NewService(pool, workspace.NewService(workspace.NewPostgresRepository(pool)), user.NewService(user.NewPostgresRepository(pool)), billing.NewService(billing.NewPostgresRepository(pool)))
	return svc, pool
}

func TestProvisionSuspendResumeLifecycle(t *testing.T) {
	svc, pool := newDBService(t)
	defer pool.Close()
	ctx := context.Background()

	name := "Platform Test " + uuid.NewString()[:8]
	email := "owner+" + uuid.NewString()[:8] + "@example.com"
	ws, err := svc.ProvisionWorkspace(ctx, name, email, billing.TierStarter, "")
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}
	t.Cleanup(func() { cleanupWorkspace(pool, ws.ID) })

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
	if err := svc.SuspendWorkspace(ctx, uuid.New(), "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown workspace, got %v", err)
	}
}

func TestListWorkspacesFilters(t *testing.T) {
	svc, pool := newDBService(t)
	defer pool.Close()
	ctx := context.Background()

	ws, err := svc.ProvisionWorkspace(ctx, "Filter Test "+uuid.NewString()[:8], "owner+"+uuid.NewString()[:8]+"@example.com", billing.TierBusiness, "")
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}
	t.Cleanup(func() { cleanupWorkspace(pool, ws.ID) })

	// Filtering by the provisioned tier returns at least our workspace.
	got, total, err := svc.ListWorkspaces(ctx, ListFilters{Tier: billing.TierBusiness, Limit: 100})
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
	yes := true
	_, _, err = svc.ListWorkspaces(ctx, ListFilters{Suspended: &active, Limit: 1})
	if err != nil {
		t.Fatalf("ListWorkspaces active filter: %v", err)
	}
	suspendedList, _, err := svc.ListWorkspaces(ctx, ListFilters{Suspended: &yes, Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkspaces suspended filter: %v", err)
	}
	for _, s := range suspendedList {
		if s.ID == ws.ID {
			t.Errorf("active workspace appeared in suspended=true filter")
		}
	}
}

// cleanupWorkspace removes the rows created by a provisioning test in
// FK-safe order. Best-effort: failures are ignored so a cleanup hiccup
// does not mask the test result.
func cleanupWorkspace(pool *pgxpool.Pool, workspaceID uuid.UUID) {
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM usage_alert_rules WHERE workspace_id = $1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM workspace_storage_credentials WHERE workspace_id = $1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM workspace_plans WHERE workspace_id = $1`, workspaceID)
	_, _ = pool.Exec(ctx, `UPDATE workspaces SET owner_user_id = NULL WHERE id = $1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE workspace_id = $1`, workspaceID)
	_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, workspaceID)
}
