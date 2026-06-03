package workspace

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// fakeIPAllowStore is an in-memory IPAllowStore used to unit-test the
// service without Postgres. It records per-method call counts so
// tests can assert the Redis cache actually short-circuits the store
// on the hot CheckAccess path.
type fakeIPAllowStore struct {
	mu sync.Mutex

	enabled map[uuid.UUID]bool
	rules   map[uuid.UUID][]IPRule

	listCalls      int
	isEnabledCalls int

	// behaviour knobs
	addErr       error
	listErr      error
	isEnabledErr error
}

func newFakeIPAllowStore() *fakeIPAllowStore {
	return &fakeIPAllowStore{
		enabled: make(map[uuid.UUID]bool),
		rules:   make(map[uuid.UUID][]IPRule),
	}
}

func (f *fakeIPAllowStore) ListRules(_ context.Context, workspaceID uuid.UUID) ([]IPRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]IPRule, len(f.rules[workspaceID]))
	copy(out, f.rules[workspaceID])
	return out, nil
}

// AddRule mirrors PostgresIPAllowStore.AddRule's atomic semantics:
// it rejects with ErrTooManyRules once the workspace is at the cap
// and with ErrDuplicateCIDR when the (workspace_id, cidr) pair is
// already present, so the service's error mapping is exercised
// without Postgres.
func (f *fakeIPAllowStore) AddRule(_ context.Context, rule IPRule) (IPRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return IPRule{}, f.addErr
	}
	existing := f.rules[rule.WorkspaceID]
	if len(existing) >= MaxIPRulesPerWorkspace {
		return IPRule{}, ErrTooManyRules
	}
	for _, r := range existing {
		if r.CIDR == rule.CIDR {
			return IPRule{}, ErrDuplicateCIDR
		}
	}
	if rule.ID == uuid.Nil {
		rule.ID = uuid.New()
	}
	f.rules[rule.WorkspaceID] = append(f.rules[rule.WorkspaceID], rule)
	return rule, nil
}

func (f *fakeIPAllowStore) RemoveRule(_ context.Context, workspaceID, ruleID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing := f.rules[workspaceID]
	for i, r := range existing {
		if r.ID == ruleID {
			f.rules[workspaceID] = append(existing[:i], existing[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeIPAllowStore) IsEnabled(_ context.Context, workspaceID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isEnabledCalls++
	if f.isEnabledErr != nil {
		return false, f.isEnabledErr
	}
	return f.enabled[workspaceID], nil
}

func (f *fakeIPAllowStore) SetEnabled(_ context.Context, workspaceID uuid.UUID, enabled bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.enabled[workspaceID]
	f.enabled[workspaceID] = enabled
	return prev, nil
}

// newTestRedis spins up a miniredis-backed client for tests that
// exercise the cache path. Returns nil-friendly cleanup via t.Cleanup.
func newTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mustAddRule(t *testing.T, svc *IPAllowService, ws uuid.UUID, cidr string) {
	t.Helper()
	if _, err := svc.AddRule(context.Background(), ws, cidr, "", uuid.New()); err != nil {
		t.Fatalf("AddRule(%q): %v", cidr, err)
	}
}

func TestCheckAccess_DisabledAlwaysAllows(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	// A rule exists but the master switch is off — must still allow,
	// even an IP that is not in the (irrelevant) rule set.
	store.rules[ws] = []IPRule{{ID: uuid.New(), WorkspaceID: ws, CIDR: "203.0.113.0/24"}}
	svc := NewIPAllowService(store, nil)

	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("8.8.8.8")); err != nil {
		t.Fatalf("disabled allowlist should allow, got %v", err)
	}
}

func TestCheckAccess_EnabledMatchAndMiss(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	store.enabled[ws] = true
	svc := NewIPAllowService(store, nil)
	mustAddRule(t, svc, ws, "203.0.113.0/24")

	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("203.0.113.7")); err != nil {
		t.Fatalf("ip in range should be allowed, got %v", err)
	}
	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("198.51.100.1")); !errors.Is(err, ErrIPBlocked) {
		t.Fatalf("ip out of range should be blocked, got %v", err)
	}
}

func TestCheckAccess_EnabledNilIPBlocked(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	store.enabled[ws] = true
	svc := NewIPAllowService(store, nil)
	mustAddRule(t, svc, ws, "203.0.113.0/24")

	if err := svc.CheckAccess(context.Background(), ws, nil); !errors.Is(err, ErrIPBlocked) {
		t.Fatalf("nil ip with enabled allowlist must be blocked, got %v", err)
	}
}

func TestCheckAccess_EnabledNoRulesBlocksEverything(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	store.enabled[ws] = true // enabled but empty allowlist
	svc := NewIPAllowService(store, nil)

	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("8.8.8.8")); !errors.Is(err, ErrIPBlocked) {
		t.Fatalf("enabled+empty must block, got %v", err)
	}
}

func TestCheckAccess_IPv6(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	store.enabled[ws] = true
	svc := NewIPAllowService(store, nil)
	mustAddRule(t, svc, ws, "2001:db8::/32")

	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("2001:db8::1")); err != nil {
		t.Fatalf("ipv6 in range should be allowed, got %v", err)
	}
	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("2001:dead::1")); !errors.Is(err, ErrIPBlocked) {
		t.Fatalf("ipv6 out of range should be blocked, got %v", err)
	}
}

func TestAddRule_RejectsInvalidAndPrivate(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)

	cases := []struct {
		name string
		cidr string
		want error
	}{
		{"garbage", "not-a-cidr", ErrInvalidCIDR},
		{"bare-ip-no-mask", "203.0.113.5", ErrInvalidCIDR},
		{"mask-out-of-range", "203.0.113.0/40", ErrInvalidCIDR},
		{"rfc1918-10", "10.0.0.0/8", ErrPrivateCIDR},
		{"rfc1918-192", "192.168.1.0/24", ErrPrivateCIDR},
		{"rfc1918-172", "172.16.0.0/12", ErrPrivateCIDR},
		{"loopback", "127.0.0.0/8", ErrPrivateCIDR},
		{"link-local", "169.254.0.0/16", ErrPrivateCIDR},
		{"ipv6-ula", "fc00::/7", ErrPrivateCIDR},
		{"ipv6-loopback", "::1/128", ErrPrivateCIDR},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AddRule(context.Background(), ws, tc.cidr, "", uuid.New())
			if !errors.Is(err, tc.want) {
				t.Fatalf("AddRule(%q): got %v, want %v", tc.cidr, err, tc.want)
			}
		})
	}
}

func TestAddRule_CanonicalizesAndAccepts(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)

	rule, err := svc.AddRule(context.Background(), ws, "203.0.113.5/24", "office", uuid.New())
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	// Host bits must be zeroed in the stored/returned form.
	if rule.CIDR != "203.0.113.0/24" {
		t.Fatalf("cidr not canonicalized: got %q, want %q", rule.CIDR, "203.0.113.0/24")
	}
	if rule.Label != "office" {
		t.Fatalf("label: got %q", rule.Label)
	}
}

func TestAddRule_EnforcesCap(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)

	// Fill to the cap with distinct public /32s.
	for i := 0; i < MaxIPRulesPerWorkspace; i++ {
		cidr := net.IPv4(203, 0, byte(i>>8), byte(i)).String() + "/32"
		if _, err := svc.AddRule(context.Background(), ws, cidr, "", uuid.New()); err != nil {
			t.Fatalf("AddRule #%d (%s): %v", i, cidr, err)
		}
	}
	_, err := svc.AddRule(context.Background(), ws, "198.51.100.1/32", "", uuid.New())
	if !errors.Is(err, ErrTooManyRules) {
		t.Fatalf("expected ErrTooManyRules at cap, got %v", err)
	}
}

// TestAddRule_RejectsDuplicate proves the same range cannot be added
// twice for a workspace, even via a non-canonical host address
// (203.0.113.7/24 canonicalizes to 203.0.113.0/24). The second add
// must surface ErrDuplicateCIDR unwrapped so the handler can map it
// to 409, and must not grow the stored rule set.
func TestAddRule_RejectsDuplicate(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)

	if _, err := svc.AddRule(context.Background(), ws, "203.0.113.0/24", "office", uuid.New()); err != nil {
		t.Fatalf("first AddRule: %v", err)
	}
	_, err := svc.AddRule(context.Background(), ws, "203.0.113.7/24", "office-again", uuid.New())
	if !errors.Is(err, ErrDuplicateCIDR) {
		t.Fatalf("expected ErrDuplicateCIDR on duplicate range, got %v", err)
	}
	rules, err := svc.ListRules(context.Background(), ws)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("duplicate add must not grow the rule set: got %d rules, want 1", len(rules))
	}
}

func TestRemoveRule_NotFound(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)
	if err := svc.RemoveRule(context.Background(), ws, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSetEnabled_ReturnsPrevious(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)

	prev, err := svc.SetEnabled(context.Background(), ws, true)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if prev {
		t.Fatalf("first toggle previous should be false, got true")
	}
	prev, err = svc.SetEnabled(context.Background(), ws, false)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if !prev {
		t.Fatalf("second toggle previous should be true, got false")
	}
}

// TestCheckAccess_CacheHitSkipsStore proves the Redis snapshot
// short-circuits the store: after the first CheckAccess populates the
// cache, a second call must not re-read IsEnabled / ListRules.
func TestCheckAccess_CacheHitSkipsStore(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	store.enabled[ws] = true
	rdb := newTestRedis(t)
	svc := NewIPAllowService(store, rdb)
	mustAddRule(t, svc, ws, "203.0.113.0/24") // also busts cache

	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("203.0.113.7")); err != nil {
		t.Fatalf("first check: %v", err)
	}
	store.mu.Lock()
	enabledAfterFirst := store.isEnabledCalls
	listAfterFirst := store.listCalls
	store.mu.Unlock()

	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("203.0.113.7")); err != nil {
		t.Fatalf("second check: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.isEnabledCalls != enabledAfterFirst {
		t.Fatalf("cache HIT should not re-read IsEnabled: before=%d after=%d", enabledAfterFirst, store.isEnabledCalls)
	}
	if store.listCalls != listAfterFirst {
		t.Fatalf("cache HIT should not re-read ListRules: before=%d after=%d", listAfterFirst, store.listCalls)
	}
}

// TestMutationBustsCache proves a rule mutation invalidates the cache
// so a previously-blocked IP is admitted on the next check.
func TestMutationBustsCache(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	store.enabled[ws] = true
	rdb := newTestRedis(t)
	svc := NewIPAllowService(store, rdb)

	// Initially enabled with no rules: everything blocked, and the
	// deny snapshot is now cached.
	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("203.0.113.7")); !errors.Is(err, ErrIPBlocked) {
		t.Fatalf("expected block before rule add, got %v", err)
	}
	// Add a covering rule — must bust the cache.
	mustAddRule(t, svc, ws, "203.0.113.0/24")
	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("203.0.113.7")); err != nil {
		t.Fatalf("expected allow after rule add busts cache, got %v", err)
	}
}

func TestValidatePublicCIDR(t *testing.T) {
	got, err := ValidatePublicCIDR("198.51.100.128/25")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "198.51.100.128/25" {
		t.Fatalf("canonical: got %q", got)
	}
}
