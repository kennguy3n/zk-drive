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
	loadCalls      int

	// behaviour knobs
	addErr       error
	listErr      error
	isEnabledErr error
	loadErr      error
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

// RemoveRule mirrors PostgresIPAllowStore.RemoveRule's atomic
// semantics: it refuses to delete the last remaining rule while the
// allowlist is enabled (ErrCannotRemoveLastRule) so the service's
// invariant "enabled ⇒ at least one rule" is exercised without
// Postgres.
func (f *fakeIPAllowStore) RemoveRule(_ context.Context, workspaceID, ruleID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing := f.rules[workspaceID]
	for i, r := range existing {
		if r.ID == ruleID {
			if f.enabled[workspaceID] && len(existing) == 1 {
				return ErrCannotRemoveLastRule
			}
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

// LoadSnapshot mirrors PostgresIPAllowStore.LoadSnapshot: it returns
// the enabled flag and the rule CIDRs read atomically under a single
// lock acquisition, so the service's cache loader can never observe a
// torn enabled/rules view.
func (f *fakeIPAllowStore) LoadSnapshot(_ context.Context, workspaceID uuid.UUID) (bool, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls++
	if f.loadErr != nil {
		return false, nil, f.loadErr
	}
	rules := f.rules[workspaceID]
	cidrs := make([]string, 0, len(rules))
	for _, r := range rules {
		cidrs = append(cidrs, r.CIDR)
	}
	return f.enabled[workspaceID], cidrs, nil
}

// SetEnabled mirrors PostgresIPAllowStore.SetEnabled's authoritative
// guard: enabling a workspace with zero rules is refused with
// ErrNoRulesToEnable, the same check the real store performs inside its
// locked transaction.
func (f *fakeIPAllowStore) SetEnabled(_ context.Context, workspaceID uuid.UUID, enabled bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if enabled && len(f.rules[workspaceID]) == 0 {
		return false, ErrNoRulesToEnable
	}
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
		{"rfc6598-cgnat", "100.64.0.0/10", ErrPrivateCIDR},
		{"rfc6598-cgnat-subnet", "100.100.0.0/24", ErrPrivateCIDR},
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

// TestRemoveRule_LastRuleWhileEnabled verifies the symmetric guard to
// TestSetEnabled_NoRulesRejected: removing the final rule of an enabled
// allowlist is refused with ErrCannotRemoveLastRule (it would leave the
// workspace enabled with zero rules, a fail-closed outage), while
// removing a non-last rule, or any rule once the allowlist is disabled,
// succeeds.
func TestRemoveRule_LastRuleWhileEnabled(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)
	mustAddRule(t, svc, ws, "203.0.113.0/24")
	if _, err := svc.SetEnabled(context.Background(), ws, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// The single rule cannot be removed while enabled.
	only := store.rules[ws][0].ID
	if err := svc.RemoveRule(context.Background(), ws, only); !errors.Is(err, ErrCannotRemoveLastRule) {
		t.Fatalf("remove last rule while enabled: got %v want ErrCannotRemoveLastRule", err)
	}
	if len(store.rules[ws]) != 1 {
		t.Fatalf("rejected removal must not delete the rule")
	}

	// A second rule makes the first removable again (count stays >= 1).
	mustAddRule(t, svc, ws, "198.51.100.0/24")
	if err := svc.RemoveRule(context.Background(), ws, only); err != nil {
		t.Fatalf("remove non-last rule: %v", err)
	}

	// Disabling lifts the guard entirely, so the now-last rule can go.
	if _, err := svc.SetEnabled(context.Background(), ws, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	last := store.rules[ws][0].ID
	if err := svc.RemoveRule(context.Background(), ws, last); err != nil {
		t.Fatalf("remove last rule while disabled: %v", err)
	}
	if len(store.rules[ws]) != 0 {
		t.Fatalf("rule set should be empty after disabled removal")
	}
}

func TestSetEnabled_ReturnsPrevious(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)
	// A rule must exist before enabling is permitted (see
	// TestSetEnabled_NoRulesRejected).
	mustAddRule(t, svc, ws, "203.0.113.0/24")

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

// TestSetEnabled_NoRulesRejected verifies the guardrail: enabling the
// allowlist for a workspace with no rules is refused with
// ErrNoRulesToEnable, the flag is left off, and the cache is not
// touched — preventing a one-toggle self-lockout. Disabling an empty
// allowlist is always permitted.
func TestSetEnabled_NoRulesRejected(t *testing.T) {
	store := newFakeIPAllowStore()
	ws := uuid.New()
	svc := NewIPAllowService(store, nil)

	if _, err := svc.SetEnabled(context.Background(), ws, true); !errors.Is(err, ErrNoRulesToEnable) {
		t.Fatalf("enable with no rules: got %v want ErrNoRulesToEnable", err)
	}
	if store.enabled[ws] {
		t.Fatalf("flag must remain off after a rejected enable")
	}
	// Disabling is always allowed, even with no rules.
	if _, err := svc.SetEnabled(context.Background(), ws, false); err != nil {
		t.Fatalf("disable with no rules should succeed, got %v", err)
	}
	// Once a rule exists, enabling succeeds.
	mustAddRule(t, svc, ws, "203.0.113.0/24")
	if _, err := svc.SetEnabled(context.Background(), ws, true); err != nil {
		t.Fatalf("enable with a rule should succeed, got %v", err)
	}
	if !store.enabled[ws] {
		t.Fatalf("flag must be on after a successful enable")
	}
}

// TestCheckAccess_CacheHitSkipsStore proves the Redis snapshot
// short-circuits the store: after the first CheckAccess populates the
// cache, a second call must not re-read the store snapshot.
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
	loadAfterFirst := store.loadCalls
	store.mu.Unlock()
	if loadAfterFirst == 0 {
		t.Fatalf("first check (cache miss) must read the store snapshot once")
	}

	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("203.0.113.7")); err != nil {
		t.Fatalf("second check: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadCalls != loadAfterFirst {
		t.Fatalf("cache HIT should not re-read the store snapshot: before=%d after=%d", loadAfterFirst, store.loadCalls)
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

// snapshotProbeStore is an IPAllowStore whose IsEnabled and ListRules
// panic. It proves the CheckAccess/snapshot path loads allowlist state
// exclusively via the single consistent LoadSnapshot read — never the
// separate IsEnabled+ListRules pair, whose interleaving with a
// concurrent disable+clear could cache a torn {enabled, no rules}
// snapshot that fails closed and blocks the whole workspace.
type snapshotProbeStore struct {
	enabled bool
	cidrs   []string
}

func (snapshotProbeStore) ListRules(context.Context, uuid.UUID) ([]IPRule, error) {
	panic("snapshot path must not call ListRules; use LoadSnapshot")
}
func (snapshotProbeStore) AddRule(context.Context, IPRule) (IPRule, error) {
	panic("unused")
}
func (snapshotProbeStore) RemoveRule(context.Context, uuid.UUID, uuid.UUID) error {
	panic("unused")
}
func (snapshotProbeStore) IsEnabled(context.Context, uuid.UUID) (bool, error) {
	panic("snapshot path must not call IsEnabled; use LoadSnapshot")
}
func (snapshotProbeStore) SetEnabled(context.Context, uuid.UUID, bool) (bool, error) {
	panic("unused")
}
func (s snapshotProbeStore) LoadSnapshot(context.Context, uuid.UUID) (bool, []string, error) {
	return s.enabled, s.cidrs, nil
}

// TestCheckAccess_LoadsViaConsistentSnapshot proves the snapshot path
// reads enabled+CIDRs through LoadSnapshot only (the probe store panics
// if IsEnabled or ListRules is touched), guarding against a regression
// to the racy two-call load that could cache an enabled/empty snapshot.
func TestCheckAccess_LoadsViaConsistentSnapshot(t *testing.T) {
	ws := uuid.New()
	svc := NewIPAllowService(snapshotProbeStore{enabled: true, cidrs: []string{"203.0.113.0/24"}}, nil)
	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("203.0.113.7")); err != nil {
		t.Fatalf("expected allow for in-range IP, got %v", err)
	}
	if err := svc.CheckAccess(context.Background(), ws, net.ParseIP("198.51.100.7")); !errors.Is(err, ErrIPBlocked) {
		t.Fatalf("expected block for out-of-range IP, got %v", err)
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

// TestValidatePublicCIDR_RejectsReservedOverlap proves a range is
// rejected when ANY address it admits is reserved — not just when its
// base address is private. A base-only check accepts over-broad masks
// (192.0.0.0/4, 128.0.0.0/1) whose public base address hides the
// RFC1918 / CGNAT / multicast space they actually cover.
func TestValidatePublicCIDR_RejectsReservedOverlap(t *testing.T) {
	accept := []struct{ in, want string }{
		{"203.0.113.5/24", "203.0.113.0/24"},
		{"198.51.100.0/24", "198.51.100.0/24"},
		{"8.8.8.0/24", "8.8.8.0/24"},
		{"2001:db8::/48", "2001:db8::/48"},
		// 2000::/3 (2000::–3fff:…) is global-unicast space that does not
		// overlap any v6 reserved block (fc00::/7, fe80::/10, ff00::/8),
		// so a broad-but-clean v6 mask is still accepted.
		{"2000::/3", "2000::/3"},
	}
	for _, tc := range accept {
		got, err := ValidatePublicCIDR(tc.in)
		if err != nil {
			t.Fatalf("ValidatePublicCIDR(%q): unexpected err %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ValidatePublicCIDR(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	reject := []string{
		"192.0.0.0/4",    // public base 192.0.0.0 but covers 192.168.0.0/16
		"128.0.0.0/1",    // public base but covers 172.16.0.0/12 + 192.168.0.0/16
		"96.0.0.0/3",     // covers 100.64.0.0/10 CGNAT and 127.0.0.0/8 loopback
		"224.0.0.0/4",    // multicast
		"10.0.0.0/8",     // RFC1918 (base also private)
		"192.168.1.0/24", // RFC1918
		"100.64.0.0/10",  // CGNAT
		"fc00::/7",       // IPv6 ULA
	}
	for _, c := range reject {
		if _, err := ValidatePublicCIDR(c); !errors.Is(err, ErrPrivateCIDR) {
			t.Fatalf("ValidatePublicCIDR(%q) = %v, want ErrPrivateCIDR", c, err)
		}
	}

	if _, err := ValidatePublicCIDR("not-a-cidr"); !errors.Is(err, ErrInvalidCIDR) {
		t.Fatalf("malformed CIDR: got %v want ErrInvalidCIDR", err)
	}
}
