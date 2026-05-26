package collab

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// yjsTestRuntime is a process-wide singleton lazy-initialised on
// first use. Compiling the wasm module costs ~50–100 ms which is
// non-trivial when multiplied across every test case; the
// singleton amortises that across the whole package's tests.
//
// We don't Close it inside the test — Go's test runner exits the
// process after the package finishes, and a non-closed runtime
// only leaks compiled-module memory until that point.
var (
	yjsTestRuntimeOnce sync.Once
	yjsTestRuntime     *YjsRuntime
	yjsTestRuntimeErr  error
)

func getYjsTestRuntime(t *testing.T) *YjsRuntime {
	t.Helper()
	yjsTestRuntimeOnce.Do(func() {
		yjsTestRuntime, yjsTestRuntimeErr = NewYjsRuntime(context.Background())
	})
	if yjsTestRuntimeErr != nil {
		t.Fatalf("init yjs runtime: %v", yjsTestRuntimeErr)
	}
	return yjsTestRuntime
}

// makeUpdate generates a real yrs-produced v1 update for a doc
// containing the given content under clientID. Used in place of
// hand-rolled byte fixtures because the v1 wire format uses
// integer varints which the spec allows multiple equivalent
// encodings for — hard-coded bytes would silently break on a yrs
// minor-version bump.
func makeUpdate(t *testing.T, rt *YjsRuntime, clientID uint64, content string) []byte {
	t.Helper()
	u, err := rt.MakeTextUpdateForTest(context.Background(), clientID, content)
	if err != nil {
		t.Fatalf("MakeTextUpdateForTest(%d, %q): %v", clientID, content, err)
	}
	if len(u) == 0 {
		t.Fatalf("empty update from MakeTextUpdateForTest")
	}
	return u
}

// TestYjsRuntime_RoundtripEmpty exercises the simplest happy path:
// no inputs → empty doc → empty state vector. This catches gross
// wiring failures (missing exports, memory layout mismatches,
// etc.) before the more elaborate merge tests run.
func TestYjsRuntime_RoundtripEmpty(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)
	merged, err := rt.MergeUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("MergeUpdates(nil): %v", err)
	}
	sv, err := rt.EncodeStateVector(context.Background(), merged)
	if err != nil {
		t.Fatalf("EncodeStateVector: %v", err)
	}
	if len(sv) == 0 {
		t.Fatalf("expected non-empty state vector")
	}
}

// TestYjsRuntime_MergeSingleUpdate verifies the no-op merge:
// passing one update returns an update that, when applied to a
// fresh Y.Doc, reproduces the same observable document state.
// We assert via the canonical correctness check: extract the
// text from a doc after applying the merged update, compare it
// to the original content.
func TestYjsRuntime_MergeSingleUpdate(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	original := makeUpdate(t, rt, 1, "hello world")
	merged, err := rt.MergeUpdates(context.Background(), [][]byte{original})
	if err != nil {
		t.Fatalf("MergeUpdates: %v", err)
	}
	if len(merged) == 0 {
		t.Fatalf("expected non-empty merged output")
	}

	got, err := rt.ApplyAndExtractText(context.Background(), merged)
	if err != nil {
		t.Fatalf("ApplyAndExtractText: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("merged update text mismatch: got %q want %q", got, "hello world")
	}
}

// TestYjsRuntime_MergeTwoIndependentUpdates verifies that two
// updates from independent clients merge into a single update
// that, when applied to a fresh Y.Doc, contains BOTH clients'
// content. This is the headline correctness property — without
// it, compaction would lose tail deltas.
//
// CRDT semantics: two clients editing the same Y.Text from
// position 0 with no awareness of each other produce a merged
// state where both insertions are present, ordered
// deterministically by (clientID, clock). We don't pin the exact
// ordering — only that BOTH inserted strings survive the merge.
func TestYjsRuntime_MergeTwoIndependentUpdates(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	uA := makeUpdate(t, rt, 1, "alpha-")
	uB := makeUpdate(t, rt, 2, "beta!")

	merged, err := rt.MergeUpdates(context.Background(), [][]byte{uA, uB})
	if err != nil {
		t.Fatalf("MergeUpdates: %v", err)
	}

	got, err := rt.ApplyAndExtractText(context.Background(), merged)
	if err != nil {
		t.Fatalf("ApplyAndExtractText: %v", err)
	}
	gotStr := string(got)
	if !bytes.Contains(got, []byte("alpha-")) {
		t.Errorf("merged update missing alpha- content: got %q", gotStr)
	}
	if !bytes.Contains(got, []byte("beta!")) {
		t.Errorf("merged update missing beta! content: got %q", gotStr)
	}
	if len(gotStr) != len("alpha-")+len("beta!") {
		t.Errorf("merged update has unexpected total length: got %q (%d) want %d", gotStr, len(gotStr), len("alpha-")+len("beta!"))
	}
}

// TestYjsRuntime_MergedUpdateHasUnionStateVector verifies that
// the merged update's state vector includes every contributing
// client. This is the property the snapshot endpoint relies on
// to advertise the catch-up watermark to subscribing clients.
func TestYjsRuntime_MergedUpdateHasUnionStateVector(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	uA := makeUpdate(t, rt, 1, "A")
	uB := makeUpdate(t, rt, 2, "B")
	uC := makeUpdate(t, rt, 3, "C")

	merged, err := rt.MergeUpdates(context.Background(), [][]byte{uA, uB, uC})
	if err != nil {
		t.Fatalf("MergeUpdates: %v", err)
	}

	svMerged, err := rt.EncodeStateVector(context.Background(), merged)
	if err != nil {
		t.Fatalf("EncodeStateVector(merged): %v", err)
	}
	svSingle, err := rt.EncodeStateVector(context.Background(), uA)
	if err != nil {
		t.Fatalf("EncodeStateVector(single): %v", err)
	}
	// A v1 state vector encodes the per-client clocks as a varint
	// count followed by (clientID, clock) varint pairs. Adding
	// two more clients always strictly increases the encoded
	// length because each clientID is a non-zero varint that
	// requires at least one byte.
	if len(svMerged) <= len(svSingle) {
		t.Errorf("merged SV (%d) should be longer than single-client SV (%d) — merge appears to have lost clients", len(svMerged), len(svSingle))
	}
}

// TestYjsRuntime_RejectsCorruptedUpdate verifies the error path:
// a malformed update payload causes the wasm side to return the
// zero (ptr, len) sentinel, which the Go bridge surfaces as an
// error. The bridge MUST NOT panic or return a partial result.
func TestYjsRuntime_RejectsCorruptedUpdate(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	garbage := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	_, err := rt.MergeUpdates(context.Background(), [][]byte{garbage})
	if err == nil {
		t.Fatal("expected error on garbage update, got nil")
	}
}

// TestYjsRuntime_ConcurrentMerges stresses the instance pool: 32
// goroutines hammer the runtime in parallel. The pool defaults
// to 8 instances, so this exercises both the "instantiate new"
// and the "wait for idle" branches. We expect no goroutine to
// observe an error and the resulting text contents to all be
// equal (every goroutine applied the same two updates).
func TestYjsRuntime_ConcurrentMerges(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	uA := makeUpdate(t, rt, 1, "alpha-")
	uB := makeUpdate(t, rt, 2, "beta!")

	const N = 32
	var wg sync.WaitGroup
	results := make([][]byte, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			merged, err := rt.MergeUpdates(context.Background(), [][]byte{uA, uB})
			if err != nil {
				errs[idx] = err
				return
			}
			text, err := rt.ApplyAndExtractText(context.Background(), merged)
			if err != nil {
				errs[idx] = err
				return
			}
			results[idx] = text
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d errored: %v", i, e)
		}
	}
	// Every concurrent merge applied the same two updates so
	// the resulting text contents must be byte-equal — any
	// divergence indicates the instance pool leaked state
	// across calls, which would be a critical correctness bug.
	ref := results[0]
	for i := 1; i < N; i++ {
		if !bytes.Equal(ref, results[i]) {
			t.Errorf("goroutine %d text content diverged from ref: %q vs %q", i, results[i], ref)
		}
	}
}

// TestYjsRuntime_FrameUpdatesIsLengthPrefixed pins the wire
// format the wasm side expects. A wire-format drift between Go
// and Rust would surface as a parse error in the wasm and
// therefore an opaque MergeUpdates error — testing the framer
// directly makes the failure mode obvious.
func TestYjsRuntime_FrameUpdatesIsLengthPrefixed(t *testing.T) {
	t.Parallel()
	out := frameUpdates([][]byte{{0x01, 0x02}, {0x03, 0x04, 0x05}})
	want := []byte{
		0x00, 0x00, 0x00, 0x02, 0x01, 0x02,
		0x00, 0x00, 0x00, 0x03, 0x03, 0x04, 0x05,
	}
	if !bytes.Equal(out, want) {
		t.Errorf("frameUpdates wire format drift:\n got: %x\nwant: %x", out, want)
	}
}

// TestYjsRuntime_MergeIsAssociative verifies that merging
// (((A B) C)) and ((A (B C))) produces equivalent observable
// state — the associativity guarantee Yjs CRDTs provide. The
// compaction scheduler relies on this when merging an existing
// snapshot with a tail of deltas: it doesn't matter whether the
// snapshot was itself produced by an earlier merge.
func TestYjsRuntime_MergeIsAssociative(t *testing.T) {
	t.Parallel()
	rt := getYjsTestRuntime(t)

	uA := makeUpdate(t, rt, 1, "A")
	uB := makeUpdate(t, rt, 2, "B")
	uC := makeUpdate(t, rt, 3, "C")

	// Left-associative: merge(merge(A,B), C)
	ab, err := rt.MergeUpdates(context.Background(), [][]byte{uA, uB})
	if err != nil {
		t.Fatalf("MergeUpdates(A,B): %v", err)
	}
	left, err := rt.MergeUpdates(context.Background(), [][]byte{ab, uC})
	if err != nil {
		t.Fatalf("MergeUpdates(AB,C): %v", err)
	}

	// Right-associative: merge(A, merge(B,C))
	bc, err := rt.MergeUpdates(context.Background(), [][]byte{uB, uC})
	if err != nil {
		t.Fatalf("MergeUpdates(B,C): %v", err)
	}
	right, err := rt.MergeUpdates(context.Background(), [][]byte{uA, bc})
	if err != nil {
		t.Fatalf("MergeUpdates(A,BC): %v", err)
	}

	leftText, err := rt.ApplyAndExtractText(context.Background(), left)
	if err != nil {
		t.Fatalf("ApplyAndExtractText(left): %v", err)
	}
	rightText, err := rt.ApplyAndExtractText(context.Background(), right)
	if err != nil {
		t.Fatalf("ApplyAndExtractText(right): %v", err)
	}
	if !bytes.Equal(leftText, rightText) {
		t.Errorf("merge associativity violation: left=%q right=%q", leftText, rightText)
	}
}

// TestYjsRuntime_BrokenInstanceIsDiscarded verifies the
// poison-prevention guarantee: when callWithInput flags an
// instance as broken (e.g. on a wasm trap), the next release
// closes the instance rather than returning it to the idle pool,
// so a subsequent acquire instantiates a fresh module instead of
// reusing memory in an indeterminate state.
//
// We construct a dedicated YjsRuntime (rather than reusing the
// package singleton) so we can inspect / assert on the pool's
// private bookkeeping without race-affecting other tests.
func TestYjsRuntime_BrokenInstanceIsDiscarded(t *testing.T) {
	t.Parallel()
	rt, err := NewYjsRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewYjsRuntime: %v", err)
	}
	t.Cleanup(func() {
		_ = rt.Close(context.Background())
	})

	inst, err := rt.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rt.mu.Lock()
	if rt.live != 1 {
		t.Fatalf("after first acquire: live=%d want 1", rt.live)
	}
	rt.mu.Unlock()

	// Simulate callWithInput's broken-instance flagging.
	// In production this happens on fn.Call error or memory
	// Write OOB; here we set it directly because the
	// externally-observable failure modes are hard to trigger
	// deterministically without injecting allocator pressure
	// the test environment doesn't guarantee.
	inst.broken = true
	rt.release(inst)

	rt.mu.Lock()
	live := rt.live
	idleLen := len(rt.idle)
	rt.mu.Unlock()
	if live != 0 {
		t.Errorf("after broken release: live=%d want 0", live)
	}
	if idleLen != 0 {
		t.Errorf("after broken release: idle has %d entries, want 0", idleLen)
	}

	// The next acquire must produce a fresh instance \u2014
	// asserted by both the pool count rolling forward and
	// the returned pointer differing from the discarded one
	// (the discarded instance's wasm module is closed, so the
	// pool cannot have re-pooled it under any code path).
	fresh, err := rt.acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if fresh == inst {
		t.Errorf("acquire returned the discarded broken instance")
	}
	rt.release(fresh)
}

// TestYjsRuntime_CloseWaitsForInflight verifies that Close blocks
// on in-flight callers via the inflight WaitGroup. Without this,
// Close could close the wazero runtime while a callWithInput
// goroutine was mid fn.Call, yielding a misleading "module
// closed" error and (worse) racing on the wasm memory pages the
// caller was still reading.
//
// The test acquires an instance and intentionally holds it past
// the Close call, then verifies Close did not complete until the
// caller released the instance. We use a goroutine + timeout
// pattern rather than a deterministic sync.Cond because the
// guarantee we are testing is "Close blocks", which is observed
// over time.
func TestYjsRuntime_CloseWaitsForInflight(t *testing.T) {
	t.Parallel()
	rt, err := NewYjsRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewYjsRuntime: %v", err)
	}

	inst, err := rt.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	closeDone := make(chan struct{})
	go func() {
		_ = rt.Close(context.Background())
		close(closeDone)
	}()

	// Close should observe inflight > 0 and block on Wait.
	// Sleep a short interval and assert closeDone is still
	// open \u2014 the goroutine must not have completed yet.
	select {
	case <-closeDone:
		t.Fatal("Close completed before the inflight caller released; inflight WaitGroup did not block")
	case <-time.After(50 * time.Millisecond):
		// expected: Close is waiting for release
	}

	// Now release the in-flight instance \u2014 release sees
	// closed==true, decrements live and closes the instance,
	// then calls inflight.Done(), unblocking Close.
	rt.release(inst)

	select {
	case <-closeDone:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not complete within 2s after release()")
	}
}

// TestYjsRuntime_AcquireAfterCloseReturnsError verifies the
// shutdown gate: once Close starts, acquire must return the
// closed sentinel so callers fail fast instead of blocking on
// the (now-torn-down) instance pool.
func TestYjsRuntime_AcquireAfterCloseReturnsError(t *testing.T) {
	t.Parallel()
	rt, err := NewYjsRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewYjsRuntime: %v", err)
	}
	if cerr := rt.Close(context.Background()); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	_, err = rt.acquire(context.Background())
	if !errors.Is(err, ErrYjsRuntimeClosed) {
		t.Errorf("acquire after Close: got %v want %v", err, ErrYjsRuntimeClosed)
	}
}
