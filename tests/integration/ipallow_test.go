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
