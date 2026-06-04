package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// TestIPAllowlist_CapHoldsUnderConcurrency proves the per-workspace
// rule cap is enforced atomically against real Postgres: even when
// far more concurrent AddRule calls than the cap race in at once,
// exactly MaxIPRulesPerWorkspace land and the rest are rejected with
// ErrTooManyRules.
//
// This is the regression test for the TOCTOU race a count-then-insert
// (or a count CTE under READ COMMITTED) leaves open — two callers
// reading count = cap-1 from independent snapshots and both
// inserting. The store closes it with a SELECT ... FOR UPDATE lock on
// the workspace row, which this test exercises end-to-end.
func TestIPAllowlist_CapHoldsUnderConcurrency(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("AllowCap", "cap@ipallow.test", "Cappy", "password-cap")
	wsID := uuid.MustParse(tok.WorkspaceID)
	userID := uuid.MustParse(tok.UserID)

	store := workspace.NewPostgresIPAllowStore(env.pool)
	svc := workspace.NewIPAllowService(store, nil)

	const attempts = workspace.MaxIPRulesPerWorkspace + 20
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok       int
		capErr   int
		otherErr error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct public /32s (TEST-NET-3, 203.0.113.0/24 has
			// 256 hosts — comfortably more than `attempts`).
			cidr := fmt.Sprintf("203.0.113.%d/32", i)
			_, err := svc.AddRule(context.Background(), wsID, cidr, "", userID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, workspace.ErrTooManyRules):
				capErr++
			default:
				otherErr = err
			}
		}(i)
	}
	wg.Wait()

	if otherErr != nil {
		t.Fatalf("unexpected AddRule error: %v", otherErr)
	}
	if ok != workspace.MaxIPRulesPerWorkspace {
		t.Fatalf("accepted %d rules, want exactly %d (cap leaked under concurrency)", ok, workspace.MaxIPRulesPerWorkspace)
	}
	if capErr != attempts-workspace.MaxIPRulesPerWorkspace {
		t.Fatalf("rejected %d with ErrTooManyRules, want %d", capErr, attempts-workspace.MaxIPRulesPerWorkspace)
	}

	// Authoritative DB count must equal the cap — never exceed it.
	var stored int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM workspace_ip_allowlist WHERE workspace_id = $1", wsID,
	).Scan(&stored); err != nil {
		t.Fatalf("count stored rules: %v", err)
	}
	if stored != workspace.MaxIPRulesPerWorkspace {
		t.Fatalf("stored %d rules, want exactly %d", stored, workspace.MaxIPRulesPerWorkspace)
	}
}

// TestIPAllowlist_RejectsDuplicateCIDR proves the
// uq_ip_allowlist_ws_cidr UNIQUE constraint rejects a repeat of the
// same canonical range — both on a plain sequential re-add (via a
// non-canonical host address that normalizes to the same network) and
// when many adds of the identical range race concurrently, where
// exactly one wins and the rest see ErrDuplicateCIDR.
func TestIPAllowlist_RejectsDuplicateCIDR(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("AllowDup", "dup@ipallow.test", "Dupy", "password-dup")
	wsID := uuid.MustParse(tok.WorkspaceID)
	userID := uuid.MustParse(tok.UserID)

	store := workspace.NewPostgresIPAllowStore(env.pool)
	svc := workspace.NewIPAllowService(store, nil)

	// Sequential: second add of the same network (different host
	// bits, different label) is a duplicate.
	if _, err := svc.AddRule(context.Background(), wsID, "198.51.100.0/24", "hq", userID); err != nil {
		t.Fatalf("first AddRule: %v", err)
	}
	if _, err := svc.AddRule(context.Background(), wsID, "198.51.100.200/24", "hq-dup", userID); !errors.Is(err, workspace.ErrDuplicateCIDR) {
		t.Fatalf("expected ErrDuplicateCIDR on re-add, got %v", err)
	}

	// Concurrent: a thundering herd of identical adds for a fresh
	// range — exactly one must succeed.
	const attempts = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok       int
		dupErr   int
		otherErr error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.AddRule(context.Background(), wsID, "203.0.113.0/24", "race", userID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, workspace.ErrDuplicateCIDR):
				dupErr++
			default:
				otherErr = err
			}
		}()
	}
	wg.Wait()

	if otherErr != nil {
		t.Fatalf("unexpected AddRule error: %v", otherErr)
	}
	if ok != 1 {
		t.Fatalf("accepted %d identical adds, want exactly 1", ok)
	}
	if dupErr != attempts-1 {
		t.Fatalf("rejected %d as duplicate, want %d", dupErr, attempts-1)
	}

	var stored int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM workspace_ip_allowlist WHERE workspace_id = $1 AND cidr = '203.0.113.0/24'", wsID,
	).Scan(&stored); err != nil {
		t.Fatalf("count stored rules: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored %d copies of the racing range, want exactly 1", stored)
	}
}

// TestIPAllowlist_EnableRemoveLastRuleInvariant proves the safety
// invariant "ip_allowlist_enabled ⇒ at least one rule" holds against
// real Postgres, both sequentially and under a SetEnabled(true) /
// RemoveRule(last) race.
//
// The two mutations both take the workspaces row's FOR UPDATE lock, so
// they serialize. Whichever wins, the loser sees the post-commit state:
// if SetEnabled enables first, RemoveRule refuses the now-last rule
// (ErrCannotRemoveLastRule); if RemoveRule deletes first, SetEnabled
// counts zero rules and refuses to enable (ErrNoRulesToEnable). Either
// way the workspace is never left enabled with an empty allowlist — the
// fail-closed, workspace-wide outage this guards against.
func TestIPAllowlist_EnableRemoveLastRuleInvariant(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("AllowInv", "inv@ipallow.test", "Invy", "password-inv")
	wsID := uuid.MustParse(tok.WorkspaceID)
	userID := uuid.MustParse(tok.UserID)
	ctx := context.Background()

	store := workspace.NewPostgresIPAllowStore(env.pool)
	svc := workspace.NewIPAllowService(store, nil)

	// Sequential semantics first.
	rule, err := svc.AddRule(ctx, wsID, "203.0.113.0/24", "hq", userID)
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if _, err := svc.SetEnabled(ctx, wsID, true); err != nil {
		t.Fatalf("enable with a rule: %v", err)
	}
	if err := svc.RemoveRule(ctx, wsID, rule.ID); !errors.Is(err, workspace.ErrCannotRemoveLastRule) {
		t.Fatalf("remove last rule while enabled: got %v want ErrCannotRemoveLastRule", err)
	}
	if _, err := svc.SetEnabled(ctx, wsID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := svc.RemoveRule(ctx, wsID, rule.ID); err != nil {
		t.Fatalf("remove last rule while disabled: %v", err)
	}

	// Concurrency: repeatedly seed exactly one rule with the flag off,
	// then race an enable against removing that rule. The invariant
	// must hold on every iteration.
	const rounds = 40
	for i := 0; i < rounds; i++ {
		r, err := svc.AddRule(ctx, wsID, fmt.Sprintf("198.51.100.%d/32", i), "", userID)
		if err != nil {
			t.Fatalf("round %d seed AddRule: %v", i, err)
		}

		var wg sync.WaitGroup
		var enableErr, removeErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, enableErr = svc.SetEnabled(ctx, wsID, true) }()
		go func() { defer wg.Done(); removeErr = svc.RemoveRule(ctx, wsID, r.ID) }()
		wg.Wait()

		// Each call either succeeds or fails with its expected guard
		// error; no unexpected (e.g. deadlock/serialization) errors.
		if enableErr != nil && !errors.Is(enableErr, workspace.ErrNoRulesToEnable) {
			t.Fatalf("round %d: unexpected SetEnabled error: %v", i, enableErr)
		}
		if removeErr != nil && !errors.Is(removeErr, workspace.ErrCannotRemoveLastRule) {
			t.Fatalf("round %d: unexpected RemoveRule error: %v", i, removeErr)
		}

		var enabled bool
		var count int
		if err := env.pool.QueryRow(ctx,
			`SELECT w.ip_allowlist_enabled,
			        (SELECT count(*) FROM workspace_ip_allowlist WHERE workspace_id = w.id)
			 FROM workspaces w WHERE w.id = $1`, wsID,
		).Scan(&enabled, &count); err != nil {
			t.Fatalf("round %d: read state: %v", i, err)
		}
		if enabled && count == 0 {
			t.Fatalf("round %d: invariant violated — enabled with zero rules", i)
		}

		// Reset to a clean (disabled, empty) baseline for the next round.
		if _, err := svc.SetEnabled(ctx, wsID, false); err != nil {
			t.Fatalf("round %d: reset disable: %v", i, err)
		}
		if _, err := env.pool.Exec(ctx,
			"DELETE FROM workspace_ip_allowlist WHERE workspace_id = $1", wsID,
		); err != nil {
			t.Fatalf("round %d: reset delete: %v", i, err)
		}
	}
}

// TestIPAllowlist_LoadSnapshotConsistent proves the cache loader's
// snapshot read can never observe a torn {enabled, no rules} view —
// the fail-closed state that would 403 every data-plane request for
// the workspace until the cache TTL expires.
//
// Two properties combine to guarantee this:
//   - LoadSnapshot reads the flag and the CIDRs in a SINGLE statement,
//     so under READ COMMITTED it sees one consistent database snapshot
//     (never enabled=true paired with a rule set a *later* delete
//     emptied).
//   - RemoveRule refuses to delete the last rule while enabled, so the
//     only way to empty the rule set is to disable first. A snapshot
//     that observes zero rules therefore also observes enabled=false
//     (the disable happened-before the delete it can see).
//
// The race below hammers LoadSnapshot while an admin disables+clears,
// asserting the loaded snapshot is never enabled-with-zero-CIDRs.
func TestIPAllowlist_LoadSnapshotConsistent(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("AllowSnap", "snap@ipallow.test", "Snappy", "password-snap")
	wsID := uuid.MustParse(tok.WorkspaceID)
	userID := uuid.MustParse(tok.UserID)
	ctx := context.Background()

	store := workspace.NewPostgresIPAllowStore(env.pool)
	svc := workspace.NewIPAllowService(store, nil)

	// Correctness: enabled snapshot returns rules in created_at order.
	if _, err := svc.AddRule(ctx, wsID, "203.0.113.0/24", "a", userID); err != nil {
		t.Fatalf("AddRule a: %v", err)
	}
	if _, err := svc.AddRule(ctx, wsID, "198.51.100.0/24", "b", userID); err != nil {
		t.Fatalf("AddRule b: %v", err)
	}
	if _, err := svc.SetEnabled(ctx, wsID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	enabled, cidrs, err := store.LoadSnapshot(ctx, wsID)
	if err != nil {
		t.Fatalf("LoadSnapshot enabled: %v", err)
	}
	if !enabled || len(cidrs) != 2 || cidrs[0] != "203.0.113.0/24" || cidrs[1] != "198.51.100.0/24" {
		t.Fatalf("LoadSnapshot enabled = %v, cidrs = %v; want true + [203.0.113.0/24 198.51.100.0/24]", enabled, cidrs)
	}

	// Disabled snapshot yields the empty (non-nil) array, never NULL.
	if _, err := svc.SetEnabled(ctx, wsID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := env.pool.Exec(ctx, "DELETE FROM workspace_ip_allowlist WHERE workspace_id = $1", wsID); err != nil {
		t.Fatalf("clear rules: %v", err)
	}
	enabled, cidrs, err = store.LoadSnapshot(ctx, wsID)
	if err != nil {
		t.Fatalf("LoadSnapshot disabled: %v", err)
	}
	if enabled || len(cidrs) != 0 {
		t.Fatalf("LoadSnapshot disabled = %v, cidrs = %v; want false + []", enabled, cidrs)
	}

	// Missing workspace -> ErrNotFound.
	if _, _, err := store.LoadSnapshot(ctx, uuid.New()); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("LoadSnapshot missing workspace: got %v want ErrNotFound", err)
	}

	// Race: repeatedly seed (enabled, 1 rule), then concurrently spin
	// LoadSnapshot while an admin disables + removes the rule. No
	// observed snapshot may be enabled with zero CIDRs.
	const rounds = 40
	for i := 0; i < rounds; i++ {
		r, err := svc.AddRule(ctx, wsID, fmt.Sprintf("203.0.113.%d/32", i), "", userID)
		if err != nil {
			t.Fatalf("round %d seed AddRule: %v", i, err)
		}
		if _, err := svc.SetEnabled(ctx, wsID, true); err != nil {
			t.Fatalf("round %d enable: %v", i, err)
		}

		var wg sync.WaitGroup
		var readErr error
		var sawTorn bool
		stop := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				en, cs, err := store.LoadSnapshot(ctx, wsID)
				if err != nil {
					readErr = err
					return
				}
				if en && len(cs) == 0 {
					sawTorn = true
					return
				}
			}
		}()

		// Admin disable-then-remove (the only legal order: RemoveRule
		// refuses the last rule while enabled).
		if _, err := svc.SetEnabled(ctx, wsID, false); err != nil {
			t.Fatalf("round %d disable: %v", i, err)
		}
		if err := svc.RemoveRule(ctx, wsID, r.ID); err != nil {
			t.Fatalf("round %d remove: %v", i, err)
		}
		close(stop)
		wg.Wait()

		if readErr != nil {
			t.Fatalf("round %d: LoadSnapshot error: %v", i, readErr)
		}
		if sawTorn {
			t.Fatalf("round %d: observed torn snapshot — enabled with zero CIDRs", i)
		}
	}
}
