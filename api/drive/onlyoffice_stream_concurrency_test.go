package drive

import (
	"sync"
	"testing"
)

// TestWithOnlyOfficeStreamSaveConcurrency pins the builder contract:
// n <= 0 leaves the streaming-save path UNLIMITED (nil semaphore, the
// default that preserves the constant-memory path's unbounded
// concurrency), while a positive n installs a semaphore of exactly that
// capacity.
func TestWithOnlyOfficeStreamSaveConcurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		n          int
		wantNilSem bool
		wantCap    int
		wantLimit  int
	}{
		{name: "zero is unlimited", n: 0, wantNilSem: true, wantLimit: 0},
		{name: "negative is unlimited", n: -5, wantNilSem: true, wantLimit: 0},
		{name: "one installs cap of 1", n: 1, wantNilSem: false, wantCap: 1, wantLimit: 1},
		{name: "positive installs sized cap", n: 8, wantNilSem: false, wantCap: 8, wantLimit: 8},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := (&Handler{}).WithOnlyOfficeStreamSaveConcurrency(tc.n)
			if tc.wantNilSem {
				if h.onlyOfficeStreamSaveSem != nil {
					t.Fatalf("sem = non-nil, want nil (unlimited)")
				}
			} else {
				if h.onlyOfficeStreamSaveSem == nil {
					t.Fatalf("sem = nil, want sized cap %d", tc.wantCap)
				}
				if got := cap(h.onlyOfficeStreamSaveSem); got != tc.wantCap {
					t.Fatalf("sem cap = %d, want %d", got, tc.wantCap)
				}
			}
			if h.onlyOfficeStreamSaveLimit != tc.wantLimit {
				t.Fatalf("limit = %d, want %d", h.onlyOfficeStreamSaveLimit, tc.wantLimit)
			}
		})
	}
}

// TestAcquireStreamSaveSlotUnlimited verifies the default (no cap)
// admits every caller and hands back a no-op release.
func TestAcquireStreamSaveSlotUnlimited(t *testing.T) {
	t.Parallel()
	h := (&Handler{}).WithOnlyOfficeStreamSaveConcurrency(0)
	for i := 0; i < 100; i++ {
		release, ok := h.acquireStreamSaveSlot()
		if !ok {
			t.Fatalf("acquire %d: ok = false, want true (unlimited)", i)
		}
		if release == nil {
			t.Fatalf("acquire %d: release = nil, want no-op", i)
		}
		release() // must not panic
	}
}

// TestAcquireStreamSaveSlotShedsBeyondCap pins the load-shedding
// contract: with a cap of N, the first N concurrent acquires succeed and
// the (N+1)th is shed (ok=false) rather than blocking; once a slot is
// released a subsequent acquire succeeds again.
func TestAcquireStreamSaveSlotShedsBeyondCap(t *testing.T) {
	t.Parallel()
	const cap = 2
	h := (&Handler{}).WithOnlyOfficeStreamSaveConcurrency(cap)

	releases := make([]func(), 0, cap)
	for i := 0; i < cap; i++ {
		release, ok := h.acquireStreamSaveSlot()
		if !ok {
			t.Fatalf("acquire %d: ok = false, want true (within cap)", i)
		}
		releases = append(releases, release)
	}

	// Cap reached: the next acquire is shed, not blocked.
	if _, ok := h.acquireStreamSaveSlot(); ok {
		t.Fatalf("acquire beyond cap: ok = true, want false (shed)")
	}

	// Free one slot; a new acquire must now succeed.
	releases[0]()
	release, ok := h.acquireStreamSaveSlot()
	if !ok {
		t.Fatalf("acquire after release: ok = false, want true")
	}
	release()
	for _, r := range releases[1:] {
		r()
	}
}

// TestAcquireStreamSaveSlotReleaseIdempotent verifies a release frees its
// slot exactly once even if called multiple times (defer + any explicit
// call), so double-release cannot over-free and let the pool admit more
// than the configured cap.
func TestAcquireStreamSaveSlotReleaseIdempotent(t *testing.T) {
	t.Parallel()
	h := (&Handler{}).WithOnlyOfficeStreamSaveConcurrency(1)

	release, ok := h.acquireStreamSaveSlot()
	if !ok {
		t.Fatal("first acquire: ok = false, want true")
	}
	// Call release several times concurrently; only one receive must
	// happen, so the single slot is freed exactly once.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()

	// Exactly one slot is free: one acquire succeeds, the next is shed.
	r1, ok1 := h.acquireStreamSaveSlot()
	if !ok1 {
		t.Fatal("acquire after idempotent release: ok = false, want true")
	}
	if _, ok2 := h.acquireStreamSaveSlot(); ok2 {
		t.Fatal("second acquire: ok = true, want false (cap is 1, double-release must not over-free)")
	}
	r1()
}
