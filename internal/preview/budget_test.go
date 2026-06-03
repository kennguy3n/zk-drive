package preview

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newTestBudget spins up an in-process miniredis and returns a budget
// enforcer wired to it plus a handle to advance the budget's clock.
// The clock is a pointer the test mutates so the sliding window can be
// exercised without real sleeps.
func newTestBudget(t *testing.T, limit int, window time.Duration) (*TenantPreviewBudget, *time.Time) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	b := NewTenantPreviewBudget(rdb, limit, window)
	clock := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	return b, &clock
}

func TestTenantPreviewBudget_AllowsUpToLimitThenRejects(t *testing.T) {
	b, _ := newTestBudget(t, 3, time.Hour)
	ws := uuid.New()
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		dec, err := b.Allow(ctx, ws)
		if err != nil {
			t.Fatalf("Allow #%d: unexpected error: %v", i, err)
		}
		if !dec.Allowed {
			t.Fatalf("Allow #%d: want admitted, got rejected (count=%d limit=%d)", i, dec.Count, dec.Limit)
		}
		if dec.Count != i {
			t.Fatalf("Allow #%d: want count=%d, got %d", i, i, dec.Count)
		}
	}

	dec, err := b.Allow(ctx, ws)
	if err != nil {
		t.Fatalf("Allow over-limit: unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("Allow over-limit: want rejected, got admitted")
	}
	if dec.Count != 3 || dec.Limit != 3 {
		t.Fatalf("Allow over-limit: want count=3 limit=3, got count=%d limit=%d", dec.Count, dec.Limit)
	}
}

func TestTenantPreviewBudget_PerWorkspaceIsolation(t *testing.T) {
	b, _ := newTestBudget(t, 1, time.Hour)
	ctx := context.Background()
	wsA, wsB := uuid.New(), uuid.New()

	if dec, err := b.Allow(ctx, wsA); err != nil || !dec.Allowed {
		t.Fatalf("wsA first: want admitted, got allowed=%v err=%v", dec.Allowed, err)
	}
	// wsA is now at its ceiling, but wsB has its own independent window.
	if dec, err := b.Allow(ctx, wsB); err != nil || !dec.Allowed {
		t.Fatalf("wsB first: want admitted, got allowed=%v err=%v", dec.Allowed, err)
	}
	if dec, err := b.Allow(ctx, wsA); err != nil || dec.Allowed {
		t.Fatalf("wsA second: want rejected, got allowed=%v err=%v", dec.Allowed, err)
	}
}

func TestTenantPreviewBudget_SlidingWindowDrains(t *testing.T) {
	b, clock := newTestBudget(t, 2, time.Hour)
	ctx := context.Background()
	ws := uuid.New()

	// Fill the window.
	for i := 0; i < 2; i++ {
		if dec, err := b.Allow(ctx, ws); err != nil || !dec.Allowed {
			t.Fatalf("fill #%d: want admitted, got allowed=%v err=%v", i, dec.Allowed, err)
		}
	}
	if dec, _ := b.Allow(ctx, ws); dec.Allowed {
		t.Fatalf("at ceiling: want rejected, got admitted")
	}

	// Advance the clock past the window so the earlier admissions fall
	// out of the trailing hour and the workspace can be admitted again.
	*clock = clock.Add(time.Hour + time.Minute)
	if dec, err := b.Allow(ctx, ws); err != nil || !dec.Allowed {
		t.Fatalf("after window drain: want admitted, got allowed=%v err=%v", dec.Allowed, err)
	}
}

func TestTenantPreviewBudget_NilReceiverAlwaysAdmits(t *testing.T) {
	var b *TenantPreviewBudget // nil: Redis not configured
	dec, err := b.Allow(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("nil budget Allow: unexpected error: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("nil budget Allow: want admitted, got rejected")
	}
	if b.Limit() != 0 {
		t.Fatalf("nil budget Limit: want 0, got %d", b.Limit())
	}
}

func TestNewTenantPreviewBudget_NilClientAndDefaults(t *testing.T) {
	if b := NewTenantPreviewBudget(nil, 10, time.Hour); b != nil {
		t.Fatalf("nil redis client: want nil enforcer, got %#v", b)
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	b := NewTenantPreviewBudget(rdb, 0, 0)
	if b.limit != DefaultBudgetPerWorkspaceHour {
		t.Fatalf("default limit: want %d, got %d", DefaultBudgetPerWorkspaceHour, b.limit)
	}
	if b.window != DefaultBudgetWindow {
		t.Fatalf("default window: want %s, got %s", DefaultBudgetWindow, b.window)
	}
}

func TestBudgetBackoff_ExponentialCappedAtFiveMinutes(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, budgetBackoffBase}, // clamped to attempt 1
		{1, budgetBackoffBase}, // 15s
		{2, 30 * time.Second},  // 15s * 2
		{3, 60 * time.Second},  // 15s * 4
		{4, 120 * time.Second}, // 15s * 8
		{5, 240 * time.Second}, // 15s * 16
		{6, MaxBudgetBackoff},  // 15s * 32 = 480s -> capped at 300s
		{20, MaxBudgetBackoff}, // far past the cap
	}
	for _, tc := range cases {
		if got := BudgetBackoff(tc.attempt); got != tc.want {
			t.Errorf("BudgetBackoff(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
	if MaxBudgetBackoff != 5*time.Minute {
		t.Fatalf("MaxBudgetBackoff = %s, want 5m (task requirement)", MaxBudgetBackoff)
	}
}
