package permission

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// fakeRepository is a recording in-memory implementation of
// Repository used to verify the cache layer's call-count semantics
// (HIT must not delegate; MISS must delegate exactly once; BUST
// must invalidate). Each method increments a per-method counter so
// tests can assert how many times the delegate was consulted.
type fakeRepository struct {
	mu sync.Mutex

	checkAccessCalls            int
	checkAccessInheritanceCalls int
	createCalls                 int
	deleteCalls                 int

	// behaviour knobs
	checkAccessResult bool
	checkAccessErr    error
	inheritanceResult bool
	inheritanceErr    error
	createErr         error
	deleteErr         error
}

func (f *fakeRepository) Create(ctx context.Context, p *Permission) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return f.createErr
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	p.CreatedAt = time.Now()
	return nil
}

func (f *fakeRepository) GetByID(ctx context.Context, workspaceID, permID uuid.UUID) (*Permission, error) {
	return nil, ErrNotFound
}

func (f *fakeRepository) ListByResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]*Permission, error) {
	return nil, nil
}

func (f *fakeRepository) ListByGrantee(ctx context.Context, workspaceID uuid.UUID, granteeType string, granteeID uuid.UUID) ([]*Permission, error) {
	return nil, nil
}

func (f *fakeRepository) Delete(ctx context.Context, workspaceID, permID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	return f.deleteErr
}

func (f *fakeRepository) CheckAccess(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkAccessCalls++
	return f.checkAccessResult, f.checkAccessErr
}

func (f *fakeRepository) CheckAccessWithInheritance(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkAccessInheritanceCalls++
	return f.inheritanceResult, f.inheritanceErr
}

// snapshotCalls returns a copy of the per-method call counters
// under the mutex.
func (f *fakeRepository) snapshotCalls() (check, inh, create, del int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkAccessCalls, f.checkAccessInheritanceCalls, f.createCalls, f.deleteCalls
}

// recordingObserver buffers RecordCacheOp invocations so tests can
// assert the observability counter sequence. Concurrency-safe.
type recordingObserver struct {
	mu      sync.Mutex
	records []cacheOpRecord
}

type cacheOpRecord struct {
	layer, op, result string
}

func (r *recordingObserver) RecordCacheOp(layer, op, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, cacheOpRecord{layer, op, result})
}

func (r *recordingObserver) count(layer, op, result string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.records {
		if rec.layer == layer && rec.op == op && rec.result == result {
			n++
		}
	}
	return n
}

// newTestRedis spins up a miniredis instance and a connected
// go-redis client. Mirrors the helper in api/middleware/
// ratelimit_redis_test.go so the same PING-until-ready discipline
// applies — miniredis.Run can return before the accept goroutine
// is scheduled on heavily-loaded CI runners.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := c.Ping(ctx).Err()
		cancel()
		if err == nil {
			return mr, c
		}
		if time.Now().After(deadline) {
			t.Fatalf("miniredis ping never succeeded: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCachedRepository_HitAfterMiss verifies the canonical cache
// shape: the first call misses (delegate hit + cache fill); the
// second call with identical arguments hits (delegate untouched,
// observer records hit).
func TestCachedRepository_HitAfterMiss(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{checkAccessResult: true, inheritanceResult: true}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	allowed1, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !allowed1 {
		t.Fatal("expected allow on first call (fake returns true)")
	}
	allowed2, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !allowed2 {
		t.Fatal("expected allow on second (cached) call")
	}

	_, inhCalls, _, _ := fake.snapshotCalls()
	if inhCalls != 1 {
		t.Errorf("delegate inheritance calls: got %d, want 1 (cache hit must not delegate)", inhCalls)
	}
	if obs.count(layerPerm, opRead, resultMiss) != 1 {
		t.Errorf("expected exactly one miss; got records %v", obs.records)
	}
	if obs.count(layerPerm, opRead, resultHit) != 1 {
		t.Errorf("expected exactly one hit; got records %v", obs.records)
	}
	if obs.count(layerPerm, opWrite, resultOK) != 1 {
		t.Errorf("expected exactly one cache write; got records %v", obs.records)
	}
}

// TestCachedRepository_NegativeCaching verifies the deny outcome
// is cached symmetrically to the allow outcome — a probing
// attacker repeatedly checking an unauthorised resource must hit
// the cache, not the DB.
func TestCachedRepository_NegativeCaching(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{checkAccessResult: false, inheritanceResult: false}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if allowed {
			t.Fatalf("call %d: expected deny", i)
		}
	}

	_, inhCalls, _, _ := fake.snapshotCalls()
	if inhCalls != 1 {
		t.Errorf("delegate calls: got %d, want 1 (negative-cache must serve denies)", inhCalls)
	}
	if obs.count(layerPerm, opRead, resultNegativeHit) != 4 {
		t.Errorf("expected 4 negative_hit reads; got %d (records %v)", obs.count(layerPerm, opRead, resultNegativeHit), obs.records)
	}
}

// TestCachedRepository_BustInvalidates verifies BustWorkspace
// rolls the generation counter so the next read goes to the
// delegate, picking up the post-mutation state.
func TestCachedRepository_BustInvalidates(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{inheritanceResult: false}
	obs := &recordingObserver{}
	// Use a much shorter local-gen freshness window than the
	// default so the test doesn't wait 500ms. We do this by
	// directly invoking the bust which also updates the local
	// gen cache, so freshness staleness is moot for this test.
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	// First call — caches the deny.
	if _, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Flip the delegate's response and BUST.
	fake.mu.Lock()
	fake.inheritanceResult = true
	fake.mu.Unlock()
	c.BustWorkspace(ctx, ws)

	allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("post-bust call: %v", err)
	}
	if !allowed {
		t.Fatal("post-bust expected allow (delegate now returns true)")
	}
	_, inhCalls, _, _ := fake.snapshotCalls()
	if inhCalls != 2 {
		t.Errorf("post-bust delegate calls: got %d, want 2 (bust must invalidate)", inhCalls)
	}
	if obs.count(layerPerm, opBust, resultBust) != 1 {
		t.Errorf("expected exactly one bust counter; got %v", obs.records)
	}
}

// TestCachedRepository_TTLExpiry verifies that an entry that has
// out-lived its TTL falls through to the delegate. miniredis
// supports manual time-travel via FastForward so the test does
// not need to sleep.
func TestCachedRepository_TTLExpiry(t *testing.T) {
	mr, rdb := newTestRedis(t)
	fake := &fakeRepository{inheritanceResult: true}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 100*time.Millisecond, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	if _, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Advance miniredis past the entry TTL. The generation
	// counter has no TTL (see cache.go bustWorkspace doc) so
	// only the entry expires here, which is the behaviour
	// this test pins.
	mr.FastForward(200 * time.Millisecond)

	if _, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer); err != nil {
		t.Fatalf("post-ttl call: %v", err)
	}
	_, inhCalls, _, _ := fake.snapshotCalls()
	if inhCalls != 2 {
		t.Errorf("post-TTL delegate calls: got %d, want 2 (entry must expire)", inhCalls)
	}
}

// TestCachedRepository_GrantInvalidates verifies that the
// CachedRepository's Create method busts the cache, so a
// subsequent CheckAccess sees the new state without waiting for
// the TTL.
func TestCachedRepository_GrantInvalidates(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{inheritanceResult: false}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	if _, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer); err != nil {
		t.Fatalf("first: %v", err)
	}
	// A grant arrives — Create busts.
	if err := c.Create(ctx, &Permission{
		WorkspaceID:  ws,
		ResourceType: ResourceFile,
		ResourceID:   res,
		GranteeType:  GranteeUser,
		GranteeID:    user,
		Role:         RoleViewer,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake.mu.Lock()
	fake.inheritanceResult = true
	fake.mu.Unlock()

	allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("post-grant: %v", err)
	}
	if !allowed {
		t.Fatal("post-grant expected allow")
	}
}

// TestCachedRepository_DeleteInvalidates is the symmetric test
// for Delete — the cache layer must bust on revoke so a
// post-revoke deny is observed immediately.
func TestCachedRepository_DeleteInvalidates(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{inheritanceResult: true}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	if _, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := c.Delete(ctx, ws, uuid.New()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	fake.mu.Lock()
	fake.inheritanceResult = false
	fake.mu.Unlock()

	allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("post-delete: %v", err)
	}
	if allowed {
		t.Fatal("post-delete expected deny")
	}
}

// TestCachedRepository_RedisDownFailsOpen verifies that when the
// Redis client returns errors, the cache layer degrades to the
// delegate without surfacing the error to the caller. The cache
// is a perf accelerator; an outage must not affect availability.
func TestCachedRepository_RedisDownFailsOpen(t *testing.T) {
	mr, rdb := newTestRedis(t)
	mr.Close() // simulate Redis crash
	fake := &fakeRepository{inheritanceResult: true}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("cache must fail open: %v", err)
	}
	if !allowed {
		t.Fatal("expected allow from delegate after Redis failure")
	}
	_, inhCalls, _, _ := fake.snapshotCalls()
	if inhCalls != 1 {
		t.Errorf("delegate calls: got %d, want 1 (Redis-down must delegate exactly once)", inhCalls)
	}
	if obs.count(layerPerm, opRead, resultError) == 0 {
		t.Errorf("expected at least one error read; got records %v", obs.records)
	}
}

// TestCachedRepository_DistinctMinRolesDistinctEntries verifies
// that different minRole queries against the same (resource,
// grantee) tuple are cached separately. The flat outcome is
// already different per minRole (admin grant satisfies viewer
// but not vice-versa); the cache key must reflect that or two
// different boolean answers would alias to one entry.
func TestCachedRepository_DistinctMinRolesDistinctEntries(t *testing.T) {
	_, rdb := newTestRedis(t)
	// Return different results based on minRole — the delegate
	// is the source of truth; we just need to observe that the
	// cache wrapper preserves the per-role distinction.
	delegate := &roleAwareFake{
		responses: map[string]bool{
			RoleViewer: true,
			RoleEditor: false,
			RoleAdmin:  false,
		},
	}
	c := NewCachedRepository(delegate, rdb, 30*time.Second, nil)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	for _, role := range []string{RoleViewer, RoleEditor, RoleAdmin} {
		want := delegate.responses[role]
		got, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, role)
		if err != nil {
			t.Fatalf("first call %s: %v", role, err)
		}
		if got != want {
			t.Errorf("first call %s: got %v want %v", role, got, want)
		}
		// Second call should hit cache and return the same.
		got2, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, role)
		if err != nil {
			t.Fatalf("second call %s: %v", role, err)
		}
		if got2 != want {
			t.Errorf("second call %s: got %v want %v", role, got2, want)
		}
	}
	if delegate.calls.Load() != 3 {
		t.Errorf("delegate calls: got %d, want 3 (one per distinct minRole)", delegate.calls.Load())
	}
}

// roleAwareFake responds to CheckAccessWithInheritance based on
// the minRole passed in so the distinct-minRole test can verify
// each role's cache key resolves independently.
type roleAwareFake struct {
	responses map[string]bool
	calls     atomic.Int64
}

func (r *roleAwareFake) Create(context.Context, *Permission) error { return nil }
func (r *roleAwareFake) GetByID(context.Context, uuid.UUID, uuid.UUID) (*Permission, error) {
	return nil, ErrNotFound
}
func (r *roleAwareFake) ListByResource(context.Context, uuid.UUID, string, uuid.UUID) ([]*Permission, error) {
	return nil, nil
}
func (r *roleAwareFake) ListByGrantee(context.Context, uuid.UUID, string, uuid.UUID) ([]*Permission, error) {
	return nil, nil
}
func (r *roleAwareFake) Delete(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (r *roleAwareFake) CheckAccess(ctx context.Context, ws uuid.UUID, rt string, ri uuid.UUID, gt string, gi uuid.UUID, role string) (bool, error) {
	r.calls.Add(1)
	return r.responses[role], nil
}
func (r *roleAwareFake) CheckAccessWithInheritance(ctx context.Context, ws uuid.UUID, rt string, ri uuid.UUID, gt string, gi uuid.UUID, role string) (bool, error) {
	r.calls.Add(1)
	return r.responses[role], nil
}

// TestCachedRepository_FlatAndInheritanceDistinct verifies the
// flat (CheckAccess) and inheritance (CheckAccessWithInheritance)
// paths use distinct cache keys so a flat-cache hit can never
// accidentally satisfy an inheritance query (their resolution
// semantics differ — most-specific-wins for inheritance, max-of-
// direct for flat).
func TestCachedRepository_FlatAndInheritanceDistinct(t *testing.T) {
	_, rdb := newTestRedis(t)
	// Configure delegate so flat says deny, inheritance says
	// allow. If the cache aliased keys, the second call would
	// read the wrong cached value.
	fake := &fakeRepository{checkAccessResult: false, inheritanceResult: true}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	flat, err := c.CheckAccess(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if flat {
		t.Fatal("flat must return deny (delegate said false)")
	}
	inh, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("inh: %v", err)
	}
	if !inh {
		t.Fatal("inheritance must return allow (delegate said true) — flat-cache must not satisfy inheritance query")
	}
	check, inhCalls, _, _ := fake.snapshotCalls()
	if check != 1 || inhCalls != 1 {
		t.Errorf("expected 1 check, 1 inh; got %d, %d", check, inhCalls)
	}
}

// TestCachedRepository_DelegateErrorNotCached verifies a
// transient delegate error does NOT poison the cache. A second
// call after the error subsides must reach the delegate again
// (since the first error MUST NOT have been cached).
func TestCachedRepository_DelegateErrorNotCached(t *testing.T) {
	_, rdb := newTestRedis(t)
	dbErr := errors.New("db down")
	fake := &fakeRepository{inheritanceErr: dbErr}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	if _, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer); !errors.Is(err, dbErr) {
		t.Fatalf("expected db err, got %v", err)
	}
	// Delegate recovers.
	fake.mu.Lock()
	fake.inheritanceErr = nil
	fake.inheritanceResult = true
	fake.mu.Unlock()

	allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("post-recover: %v", err)
	}
	if !allowed {
		t.Fatal("post-recover expected allow")
	}
	_, inhCalls, _, _ := fake.snapshotCalls()
	if inhCalls != 2 {
		t.Errorf("delegate calls: got %d, want 2 (errors must not be cached)", inhCalls)
	}
}

// TestService_WithCache_NilRedisIsNoop verifies the wiring
// helper preserves the un-cached repo when redis is nil — the
// REDIS_URL-unset path must not panic.
func TestService_WithCache_NilRedisIsNoop(t *testing.T) {
	fake := &fakeRepository{inheritanceResult: true}
	svc := NewService(fake).WithCache(nil, 30*time.Second, nil)

	if svc.repo != fake {
		t.Errorf("expected un-cached repo; got %T", svc.repo)
	}
}

// TestService_WithCache_TypedNilRedisIsNoop covers the Go
// interface pitfall where a typed-nil concrete pointer wrapped
// in an interface value is NOT == nil. A naïve `if rdb == nil`
// check would skip the guard and a CachedRepository would be
// constructed around a nil *redis.Client, which would panic at
// the first GET call. The fix is to also detect typed-nil via
// reflect; this test pins the contract.
func TestService_WithCache_TypedNilRedisIsNoop(t *testing.T) {
	var typedNil *redis.Client // (*redis.Client)(nil)
	fake := &fakeRepository{inheritanceResult: true}
	svc := NewService(fake).WithCache(typedNil, 30*time.Second, nil)

	if svc.repo != fake {
		t.Errorf("expected un-cached repo on typed-nil *redis.Client; got %T", svc.repo)
	}
}

// TestService_WithCache_ZeroTTLIsNoop guards against an
// accidentally-misconfigured TTL: zero ttl would translate to
// "no expiry" on Redis, leaking entries forever. The wiring
// helper rejects it and leaves the repo un-cached so the cache
// can't quietly malfunction.
func TestService_WithCache_ZeroTTLIsNoop(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{inheritanceResult: true}
	svc := NewService(fake).WithCache(rdb, 0, nil)
	if svc.repo != fake {
		t.Errorf("expected un-cached repo on zero TTL; got %T", svc.repo)
	}
}

// TestService_BustWorkspace_NoCacheNoop verifies BustWorkspace on
// a service whose repo is the un-cached PostgresRepository (or
// any non-CachedRepository) is a no-op rather than a panic.
func TestService_BustWorkspace_NoCacheNoop(t *testing.T) {
	fake := &fakeRepository{}
	svc := NewService(fake)
	// Should be a complete no-op — no panic, no errors.
	svc.BustWorkspace(context.Background(), uuid.New())
}

// TestCachedRepository_ConcurrentReadsSafe stress-tests the
// concurrent read path: 50 goroutines hammering the same key.
// Verifies (a) no race detector trips, (b) the delegate is called
// at most a small number of times (each goroutine that loses the
// race to fill the cache still fetches from the delegate — we
// don't implement a "single-flight" coalescer in v1).
func TestCachedRepository_ConcurrentReadsSafe(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{inheritanceResult: true}
	obs := &recordingObserver{}
	c := NewCachedRepository(fake, rdb, 30*time.Second, obs)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
			if err != nil {
				t.Errorf("concurrent: %v", err)
			}
			if !allowed {
				t.Error("concurrent: expected allow")
			}
		}()
	}
	wg.Wait()
	// We don't assert delegate calls == 1 here because we
	// intentionally do not implement single-flight in v1; the
	// invariant is that the answer is correct and the test
	// terminates cleanly under -race. The hit-vs-miss split is
	// observable via the observer counters but the exact ratio
	// depends on Redis scheduling.
	// The total must cover every goroutine — hit OR miss OR
	// resultError. Folding resultError in is defensive: under
	// miniredis (deterministic, in-process) the error path is
	// unreachable so empirically the count is zero, but the
	// assertion stays valid if the test ever runs against a
	// real Redis with intermittent timeouts under heavy CI
	// load. The fail-open contract MUST manifest as one of
	// the three observer outcomes; a missing record would be
	// a real bug.
	miss := obs.count(layerPerm, opRead, resultMiss)
	hit := obs.count(layerPerm, opRead, resultHit)
	errs := obs.count(layerPerm, opRead, resultError)
	if miss+hit+errs != n {
		t.Errorf("expected %d total reads; got hit=%d miss=%d err=%d (records=%d)",
			n, hit, miss, errs, len(obs.records),
		)
	}
}

// TestCachedRepository_GenerationCounterLocalCacheStale verifies
// that after the local generation cache goes stale (past
// generationStaleAfter), a subsequent read re-fetches the
// generation from Redis. We force the staleness by manually
// re-stamping the local entry's fetchedAt timestamp in the past.
func TestCachedRepository_GenerationCounterLocalCacheStale(t *testing.T) {
	_, rdb := newTestRedis(t)
	fake := &fakeRepository{inheritanceResult: true}
	c := NewCachedRepository(fake, rdb, 30*time.Second, nil)

	ws := uuid.New()
	res := uuid.New()
	user := uuid.New()
	ctx := context.Background()

	// Prime the generation cache.
	if _, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Simulate a bust on a sibling replica: INCR the
	// generation directly (bypassing this CachedRepository's
	// local-cache update path), then age the local generation
	// entry past staleness so the next read refetches.
	if _, err := rdb.Incr(ctx, generationKey(ws)).Result(); err != nil {
		t.Fatalf("sibling incr: %v", err)
	}
	c.genMu.Lock()
	if entry, ok := c.gen[ws]; ok {
		entry.fetchedAt = time.Now().Add(-2 * generationStaleAfter)
	}
	c.genMu.Unlock()

	// Flip the delegate so we can see whether the cache
	// resolves under old or new gen.
	fake.mu.Lock()
	fake.inheritanceResult = false
	fake.mu.Unlock()

	allowed, err := c.CheckAccessWithInheritance(ctx, ws, ResourceFile, res, GranteeUser, user, RoleViewer)
	if err != nil {
		t.Fatalf("post-stale: %v", err)
	}
	if allowed {
		t.Fatal("post-stale-gen expected deny (the new gen must have invalidated the entry)")
	}
}
