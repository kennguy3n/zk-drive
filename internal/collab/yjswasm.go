package collab

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ymergeWASM is the compiled Rust wasm module exporting `alloc`,
// `dealloc`, `merge_updates`, and `encode_state_vector`. Source
// lives at internal/collab/wasm/ymerge/; build script lives at
// internal/collab/wasm/build.sh.
//
// We embed the binary rather than loading from disk so the
// deployment artefact stays a single Go binary — no
// runtime-mounted resource dirs to worry about in containers /
// systemd units. The .wasm is committed to the repo so CI (which
// has no Rust toolchain) does not need to rebuild it; rebuild
// locally with `internal/collab/wasm/build.sh` and commit the
// updated binary alongside the Rust source change.
//
//go:embed wasm/ymerge.wasm
var ymergeWASM []byte

// YjsRuntime is a pool of wazero module instances that can apply
// Yjs updates via the embedded Rust wasm. Concurrent compactions
// take an instance from the pool, use it, and return it; the
// pool grows lazily up to maxInstances and parks idle instances
// indefinitely (each instance's RSS is bounded by the wasm
// module's max memory pages and the yrs-internal state, both of
// which are released when the instance is closed).
//
// Lifetime: wired in cmd/server/main.go as a process-singleton
// alongside the collab hub, and closed on graceful shutdown.
// A nil receiver acts as a no-op fallback — every call returns
// an error so the caller can decide whether to skip the fold or
// fall back to OpaqueConcatFold.
type YjsRuntime struct {
	rt           wazero.Runtime
	compiled     wazero.CompiledModule
	maxInstances int

	mu sync.Mutex
	// idle is a LIFO stack of returned instances available for
	// the next acquire. An instance is in exactly one of two
	// states: idle (sitting in this slice) or in-use (handed
	// out to a caller, will be returned via release / discard).
	// We deliberately do NOT keep a parallel "all" slice that
	// Close iterates because closing an in-use instance from
	// Close while the caller is mid-fn.Call would race the
	// wazero memory and trap the wasm. Close instead waits for
	// in-flight callers to drain via `inflight` and then closes
	// what remains in idle.
	idle []*yjsInstance
	// live counts instances that have been instantiated and
	// not yet permanently closed (i.e. not yet discarded by a
	// caller and not yet drained by Close). Used to gate
	// further instantiation against maxInstances. Decremented
	// when release closes a broken instance, when discard
	// closes a known-broken instance, and when Close drains
	// the idle slice.
	live int
	// inflight tracks every instance currently checked out via
	// acquire and not yet returned via release or discard. Close
	// waits on this WaitGroup so the wazero runtime is never
	// torn down while a caller is mid fn.Call.
	inflight sync.WaitGroup
	// closed flips to true once Close starts. acquire refuses
	// to hand out instances after closed; release / discard
	// close their instance directly instead of re-pooling.
	closed    bool
	closeOnce sync.Once
	// acquireBackoff is the sleep applied between acquire
	// retries when the pool is at capacity. Constant in
	// production; tests override to zero to keep latency
	// predictable when forcing serial execution.
	acquireBackoff time.Duration
}

// defaultAcquireBackoff is the inter-retry sleep duration
// applied by acquire() when the pool is exhausted. The previous
// implementation used `default:` in the select which spun the
// retry loop at full CPU until an instance was released; a 1ms
// sleep yields the scheduler so the goroutine waiting for an
// instance contributes ~0% CPU instead of saturating a core.
// 1ms is small enough that pool-release latency is not
// noticeably amplified (release-then-reacquire roundtrip in
// realistic workloads is microseconds).
const defaultAcquireBackoff = time.Millisecond

// yjsInstance wraps a single instantiated wasm module along with
// the function handles we call repeatedly. Cached so each
// merge/encode op only pays one map-lookup per export.
//
// The `broken` flag is set by callWithInput when a wasm-level
// trap (fn.Call returning a non-nil error) makes this instance's
// memory state untrustworthy. release inspects the flag and
// closes the instance instead of re-pooling so a single OOM trap
// or yrs panic cannot poison subsequent compactions on the same
// process. The flag is only ever written / read by the goroutine
// that holds the instance via acquire, so no synchronisation is
// needed.
type yjsInstance struct {
	mod                 api.Module
	mem                 api.Memory
	alloc               api.Function
	dealloc             api.Function
	mergeUpdates        api.Function
	encodeStateVec      api.Function
	applyAndExtractText api.Function
	makeTextUpdate      api.Function
	broken              bool
}

// DefaultYjsRuntimeMaxInstances caps how many wasm instances live
// in parallel. Each instance carries its own linear memory pages
// + yrs CRDT state for the current document being folded, so
// memory cost scales linearly with concurrency. 8 is enough for
// typical replica capacity (compaction is bounded by the
// document.MaxSnapshotTailDeltas pull); operators can tune via
// the WithMaxInstances option if their workload demands more
// concurrent compactions.
const DefaultYjsRuntimeMaxInstances = 8

// NewYjsRuntime compiles the embedded wasm module and returns a
// runtime ready to serve concurrent merge / state-vector
// requests. The compile step is the expensive part (parsing the
// wasm bytecode, building the JIT cache); per-instance startup
// is a millisecond-class memory init.
//
// The runtime should be created once at server boot and reused.
// Close it during graceful shutdown to release the compiled
// module cache and any idle instance pages.
//
// ctx is used only for the compile step; the returned runtime
// holds no reference to it.
func NewYjsRuntime(ctx context.Context) (*YjsRuntime, error) {
	cfg := wazero.NewRuntimeConfig().
		// WithCloseOnContextDone=false because we manage
		// lifetime explicitly via Close(); we don't want a
		// canceled compile context to leak into the running
		// instances.
		WithCloseOnContextDone(false)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)

	// The ymerge wasm is compiled to wasm32-wasip1 because yrs
	// transitively pulls getrandom for client-ID generation, and
	// the WASI random_get import is the cleanest way to satisfy
	// that on a non-JS host. We instantiate wazero's built-in
	// WASI preview1 host module so every subsequent
	// InstantiateModule call resolves the WASI imports without
	// needing a custom host. The Instantiate call exposes the
	// host module under the canonical name
	// `wasi_snapshot_preview1`; the wasm-side imports use the
	// same name.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("collab: wire wasi host: %w", err)
	}

	compiled, err := rt.CompileModule(ctx, ymergeWASM)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("collab: compile ymerge.wasm: %w", err)
	}
	return &YjsRuntime{
		rt:             rt,
		compiled:       compiled,
		maxInstances:   DefaultYjsRuntimeMaxInstances,
		acquireBackoff: defaultAcquireBackoff,
	}, nil
}

// WithMaxInstances overrides the default instance-pool ceiling.
// Useful for tests that want to force serial execution
// (maxInstances=1) and for operators tuning a high-concurrency
// deployment. A value <=0 is clamped to 1.
func (r *YjsRuntime) WithMaxInstances(n int) *YjsRuntime {
	if n < 1 {
		n = 1
	}
	r.mu.Lock()
	r.maxInstances = n
	r.mu.Unlock()
	return r
}

// WithAcquireBackoff overrides the inter-retry sleep applied by
// acquire() when the instance pool is at capacity. The production
// default (defaultAcquireBackoff = 1ms) suits the typical
// document.CompactionThreshold cadence, where instance churn is
// rare and a tiny sleep keeps the waiting goroutine off-CPU.
//
// Operators running a high-concurrency deployment with many
// long-running compactions (large managed_encrypted documents,
// many concurrent room compactions) may benefit from a longer
// backoff (e.g. 5ms-10ms) to reduce wake-up churn, or a shorter
// one for latency-sensitive setups. Tests use this to set 0 and
// force a tight retry loop without blocking on real wall-clock
// time.
//
// A value <=0 selects the test-only zero-sleep path (acquire
// still yields the scheduler via select{} on ctx.Done before
// retrying so context cancellation is honoured). The setter is
// safe to call concurrently with other public methods.
func (r *YjsRuntime) WithAcquireBackoff(d time.Duration) *YjsRuntime {
	r.mu.Lock()
	r.acquireBackoff = d
	r.mu.Unlock()
	return r
}

// Close releases the compiled module and every cached instance.
// Idempotent — safe to call multiple times from concurrent
// shutdown paths.
//
// Ordering:
//  1. Flip `closed` under r.mu so future acquire calls return
//     ErrYjsRuntimeClosed and every release path checking r.closed
//     under the same mutex sees the new value.
//  2. Snapshot + clear r.idle under the same lock, and decrement
//     `live` once per entry — those instances have no in-flight
//     caller, so we can close them immediately and the bookkeeping
//     stays consistent for any pool-stats consumer that might
//     read `live` after Close returns.
//  3. Wait on `inflight` for every currently-checked-out caller
//     to call release / discard. Each release acquires r.mu, sees
//     closed==true, decrements `live`, and closes its own instance
//     (instead of re-pooling), so by the time Wait returns no
//     instance is checked out and `live` is back to zero.
//  4. Close the wazero runtime last. At this point no callWithInput
//     can still be running because step (3) waited for it.
//
// We do not need a second idle drain after the Wait: release
// inspects r.closed under r.mu before deciding the case, so once
// r.closed=true no release can re-pool an instance to idle. The
// idleSnapshot captured in step (2) is therefore the complete set
// of idle instances that will ever exist for this runtime.
//
// This eliminates the previous design's race window where Close
// closed in-use modules concurrently with the caller's fn.Call,
// which only worked because wazero's Module.Close is idempotent
// (the wasm call would have raced into a closed runtime and
// returned a misleading error).
func (r *YjsRuntime) Close(ctx context.Context) error {
	var err error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		idleSnapshot := r.idle
		r.idle = nil
		// Decrement live for every idle instance we are
		// about to close. This keeps the live counter
		// consistent across all teardown paths (release of
		// in-flight instances also decrements live), so a
		// future pool-stats / health endpoint that reads
		// live after Close observes zero instead of a
		// stale over-count.
		r.live -= len(idleSnapshot)
		r.mu.Unlock()
		for _, inst := range idleSnapshot {
			if cerr := inst.mod.Close(ctx); cerr != nil && err == nil {
				err = cerr
			}
		}
		r.inflight.Wait()
		if cerr := r.rt.Close(ctx); cerr != nil && err == nil {
			err = cerr
		}
	})
	return err
}

// ErrYjsRuntimeClosed is returned by acquire (and the public
// MergeUpdates / EncodeStateVector / ApplyAndExtractText /
// MakeTextUpdateForTest entry points) once Close has begun.
// Callers should treat this as a permanent failure for the
// process lifetime.
var ErrYjsRuntimeClosed = errors.New("collab: YjsRuntime is closed")

// acquire pulls an instance from the idle pool or creates a new
// one (up to maxInstances). When the pool is at capacity the
// call sleeps for acquireBackoff and retries until either an
// instance becomes available, ctx is cancelled, or Close starts.
//
// The retry sleep is implemented via a timer rather than a sync.Cond:
// instance churn is rare in steady state (compactions trigger at
// the document.CompactionThreshold cadence) so the simpler
// timer-based fallback avoids the complexity of a condvar without
// the CPU-burn pathology of a `default:` spin.
//
// Acquire never returns a nil instance unless err is non-nil.
func (r *YjsRuntime) acquire(ctx context.Context) (*yjsInstance, error) {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, ErrYjsRuntimeClosed
		}
		if len(r.idle) > 0 {
			inst := r.idle[len(r.idle)-1]
			r.idle = r.idle[:len(r.idle)-1]
			r.inflight.Add(1)
			r.mu.Unlock()
			return inst, nil
		}
		if r.live < r.maxInstances {
			r.live++
			r.mu.Unlock()
			inst, err := r.instantiate(ctx)
			if err != nil {
				r.mu.Lock()
				r.live--
				r.mu.Unlock()
				return nil, err
			}
			r.mu.Lock()
			if r.closed {
				// Close ran while we were instantiating;
				// close this instance directly so it is
				// not leaked, and surface the closed error
				// to the caller. We do NOT increment
				// inflight on this path because we never
				// hand the instance out.
				r.live--
				r.mu.Unlock()
				_ = inst.mod.Close(ctx)
				return nil, ErrYjsRuntimeClosed
			}
			r.inflight.Add(1)
			r.mu.Unlock()
			return inst, nil
		}
		backoff := r.acquireBackoff
		r.mu.Unlock()
		// Pool exhausted; sleep for backoff and retry. A
		// non-zero backoff (the production default is 1ms)
		// yields the scheduler so the goroutine waiting for
		// an instance contributes ~0% CPU instead of
		// busy-waiting on a core.
		if backoff <= 0 {
			// Test path: caller opted into a zero-sleep
			// retry loop. Honour ctx cancellation between
			// iterations even though we never block.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			continue
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// release returns an instance to the pool, or closes it if the
// runtime is shutting down or the instance is flagged broken.
// Every successful acquire must be paired with exactly one
// release (or discard) so the inflight WaitGroup balances; the
// public entry points (MergeUpdates / EncodeStateVector / etc.)
// pair them via `defer r.release(inst)`.
func (r *YjsRuntime) release(inst *yjsInstance) {
	r.mu.Lock()
	switch {
	case r.closed:
		// Close ran while this instance was checked out;
		// do not re-pool it. We use context.Background
		// here because the release path is best-effort
		// cleanup invoked from a `defer` whose ctx has
		// typically been cancelled already; the wasm
		// module's Close is a pure-Go memory release that
		// does not need a live ctx.
		r.live--
		r.mu.Unlock()
		_ = inst.mod.Close(context.Background())
	case inst.broken:
		// The caller's fn.Call trapped (likely yrs OOM /
		// allocator panic) and left wasm memory in an
		// untrusted state. Closing the instance and
		// decrementing live frees the slot for a fresh
		// instantiation on the next acquire — one bad
		// input cannot poison subsequent compactions on
		// the same process.
		r.live--
		r.mu.Unlock()
		_ = inst.mod.Close(context.Background())
	default:
		r.idle = append(r.idle, inst)
		r.mu.Unlock()
	}
	r.inflight.Done()
}

func (r *YjsRuntime) instantiate(ctx context.Context) (*yjsInstance, error) {
	// Module config:
	//   - WithName(""): wazero's default name-resolution uses the
	//     wasm-defined module name from the binary's name section,
	//     which is fixed at "ymerge.wasm" for our build. That
	//     would reject a second concurrent instance with
	//     `module[ymerge.wasm] has already been instantiated`.
	//     An empty name registers each instance anonymously so
	//     the pool can grow up to maxInstances.
	//   - WithStartFunctions("_initialize"): a wasm32-wasip1
	//     reactor library (which is what `crate-type = ["cdylib"]`
	//     produces when the WASI tier-2 stdlib is linked) exposes
	//     `_initialize` rather than `_start`. wazero defaults to
	//     looking for `_start`; we explicitly point it at
	//     `_initialize` so the libc-style global ctors / TLS init
	//     run before our exported entry points. Without this,
	//     yrs's first allocation underflows uninitialised state
	//     and merge_updates returns the error sentinel.
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize")
	mod, err := r.rt.InstantiateModule(ctx, r.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("collab: instantiate ymerge: %w", err)
	}
	get := func(name string) (api.Function, error) {
		fn := mod.ExportedFunction(name)
		if fn == nil {
			return nil, fmt.Errorf("collab: ymerge missing export %q", name)
		}
		return fn, nil
	}
	mem := mod.Memory()
	if mem == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("collab: ymerge wasm exposes no memory")
	}
	alloc, err := get("alloc")
	if err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	dealloc, err := get("dealloc")
	if err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	mergeUpdates, err := get("merge_updates")
	if err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	encodeStateVec, err := get("encode_state_vector")
	if err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	applyAndExtractText, err := get("apply_and_extract_text")
	if err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	makeTextUpdate, err := get("make_text_update")
	if err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	return &yjsInstance{
		mod:                 mod,
		mem:                 mem,
		alloc:               alloc,
		dealloc:             dealloc,
		mergeUpdates:        mergeUpdates,
		encodeStateVec:      encodeStateVec,
		applyAndExtractText: applyAndExtractText,
		makeTextUpdate:      makeTextUpdate,
	}, nil
}

// callWithInput allocates a wasm-side buffer, copies `input`
// into it, invokes `fn(ptr, len)`, and returns the resulting
// (ptr, len) packed u64 unpacked into a freshly-copied Go []byte.
// All wasm-side allocations are freed before return so the
// instance is left in the same allocated state it started in.
//
// A zero (ptr, len) result from the wasm function is treated as
// an error — the Rust side returns this on parse / decode
// failures. Such a sentinel error leaves the instance in a clean
// state (every alloc the Rust function performed before returning
// the sentinel was matched by a dealloc inside the same function),
// so the caller's release path can safely re-pool the instance.
//
// A non-nil err from fn.Call OR alloc.Call OR a memory Write OOB
// indicates a wasm-level trap, an aborted allocation, or a host
// memory access into an invalid range — any of which leaves the
// instance's linear memory in an indeterminate state (the wasm
// allocator's internal book-keeping may be inconsistent with the
// pointers it has handed out). callWithInput flags such cases on
// the instance so release closes it instead of re-pooling, and
// the next acquire instantiates a fresh wasm module from the
// shared compiled cache.
func (r *YjsRuntime) callWithInput(ctx context.Context, inst *yjsInstance, fn api.Function, input []byte) ([]byte, error) {
	inputPtr, err := r.writeToWasm(ctx, inst, input)
	if err != nil {
		inst.broken = true
		return nil, err
	}
	defer r.deallocWasm(ctx, inst, inputPtr, uint32(len(input)))

	results, err := fn.Call(ctx, uint64(inputPtr), uint64(len(input)))
	if err != nil {
		inst.broken = true
		return nil, fmt.Errorf("collab: ymerge call: %w", err)
	}
	if len(results) != 1 {
		inst.broken = true
		return nil, fmt.Errorf("collab: ymerge call returned %d results, expected 1", len(results))
	}
	resultPtr := uint32(results[0] >> 32)
	resultLen := uint32(results[0] & 0xFFFFFFFF)
	if resultPtr == 0 {
		return nil, errors.New("collab: ymerge returned error (zero result ptr)")
	}
	defer r.deallocWasm(ctx, inst, resultPtr, resultLen)

	if resultLen == 0 {
		// Legal — a no-op merge of an empty input would
		// return zero bytes. Return an empty slice rather
		// than nil so the caller doesn't conflate "empty"
		// with "error".
		return []byte{}, nil
	}
	out, ok := inst.mem.Read(resultPtr, resultLen)
	if !ok {
		// A failed Read means the wasm function returned a
		// pointer/length pair that did not lie inside the
		// instance's linear memory — either a buggy Rust
		// export or memory corruption. Flag the instance
		// broken so we re-instantiate on the next acquire.
		inst.broken = true
		return nil, fmt.Errorf("collab: ymerge result OOB ptr=%d len=%d mem=%d", resultPtr, resultLen, inst.mem.Size())
	}
	// Copy out of wasm memory because the underlying buffer
	// belongs to the wasm instance — once we return, the
	// caller may use the slice past the next instance op,
	// which could overwrite this region.
	copied := make([]byte, len(out))
	copy(copied, out)
	return copied, nil
}

// writeToWasm allocates len(buf) bytes inside the wasm instance
// and copies buf into them. Returns the wasm-side pointer.
func (r *YjsRuntime) writeToWasm(ctx context.Context, inst *yjsInstance, buf []byte) (uint32, error) {
	if len(buf) == 0 {
		// alloc(0) in our Rust shim returns a sentinel
		// non-null pointer we never read from; the call
		// pattern below still passes len=0 to the entry
		// point so the dispatch is correct.
		results, err := inst.alloc.Call(ctx, 0)
		if err != nil {
			return 0, fmt.Errorf("collab: ymerge alloc(0): %w", err)
		}
		return uint32(results[0]), nil
	}
	results, err := inst.alloc.Call(ctx, uint64(len(buf)))
	if err != nil {
		return 0, fmt.Errorf("collab: ymerge alloc(%d): %w", len(buf), err)
	}
	ptr := uint32(results[0])
	if ptr == 0 {
		return 0, fmt.Errorf("collab: ymerge alloc returned null for %d bytes", len(buf))
	}
	if !inst.mem.Write(ptr, buf) {
		return 0, fmt.Errorf("collab: ymerge memory write OOB ptr=%d len=%d mem=%d", ptr, len(buf), inst.mem.Size())
	}
	return ptr, nil
}

func (r *YjsRuntime) deallocWasm(ctx context.Context, inst *yjsInstance, ptr, size uint32) {
	if ptr == 0 {
		return
	}
	_, err := inst.dealloc.Call(ctx, uint64(ptr), uint64(size))
	if err != nil {
		// A failed dealloc means the wasm allocator's
		// book-keeping is now out of sync with the pointers it
		// has handed out — a subsequent alloc on the same
		// instance may reuse a region we believed freed, or hit
		// a corrupted free-list and trap. Same indeterminate-
		// memory argument as a fn.Call trap: do NOT silently
		// swallow the error and re-pool the instance.
		//
		// Flag the instance broken so release closes it instead
		// of re-pooling, and the next acquire instantiates a
		// fresh module from the shared compiled cache. We log
		// at Warn level so operators can correlate a spike in
		// instance-replace churn with whatever upstream cause
		// produced the dealloc failure (typically a yrs OOM
		// inside the Rust call that already trapped fn.Call,
		// in which case `broken` was already set — the dealloc
		// path here covers the secondary case where alloc/Call
		// succeeded but dealloc fails alone).
		inst.broken = true
		slog.Default().Warn("collab: ymerge dealloc failed; instance flagged broken",
			"err", err,
			"ptr", ptr,
			"size", size,
		)
	}
}

// MergeUpdates applies a sequence of Yjs v1 update payloads to a
// fresh document inside the wasm instance and returns the
// compact single-update encoding of the resulting state. Order
// matters at the wire level (Yjs is order-tolerant
// semantically, but the Rust apply loop applies in the order
// the host provides).
//
// updates can be nil/empty; the result is an empty []byte in that
// case (no error). This matches the OpaqueConcatFold behaviour
// of carrying forward an initial empty state through the first
// compaction.
//
// Concurrency: safe to call from multiple goroutines; each call
// acquires a pooled instance, holds it for the duration, and
// returns it. See acquire() for the pool sizing rationale.
func (r *YjsRuntime) MergeUpdates(ctx context.Context, updates [][]byte) ([]byte, error) {
	if r == nil {
		return nil, errors.New("collab: nil YjsRuntime")
	}
	inst, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer r.release(inst)

	framed := frameUpdates(updates)
	return r.callWithInput(ctx, inst, inst.mergeUpdates, framed)
}

// ApplyAndExtractText applies a v1-encoded update to a fresh
// Y.Doc and returns the UTF-8 bytes of the Y.Text named "t".
// Exported primarily so tests can verify that a merged update
// reproduces the same observable document state as the original
// updates (the highest-fidelity correctness check we can run
// without depending on the canonical Yjs JS library at test
// time).
//
// Returns an empty []byte if the doc had no text content, an
// error if the update fails to decode/apply.
func (r *YjsRuntime) ApplyAndExtractText(ctx context.Context, update []byte) ([]byte, error) {
	if r == nil {
		return nil, errors.New("collab: nil YjsRuntime")
	}
	inst, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer r.release(inst)
	return r.callWithInput(ctx, inst, inst.applyAndExtractText, update)
}

// MakeTextUpdateForTest constructs a fresh Y.Doc with the given
// clientID, inserts `content` as text into a Y.Text named "t",
// and returns the v1-encoded update capturing the full doc state.
// Used by tests to generate real yrs-produced fixtures without
// depending on the canonical Yjs JS library — embedding magic
// bytes is fragile across yrs minor versions because v1 uses
// integer varints which the spec allows multiple equivalent
// encodings for.
//
// "ForTest" suffix because this is not the production code path
// (the production path receives updates from clients via the
// WebSocket bridge, never generates them server-side).
func (r *YjsRuntime) MakeTextUpdateForTest(ctx context.Context, clientID uint64, content string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("collab: nil YjsRuntime")
	}
	inst, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer r.release(inst)

	// Layout: 8-byte big-endian clientID then UTF-8 content.
	input := make([]byte, 8, 8+len(content))
	binary.BigEndian.PutUint64(input[:8], clientID)
	input = append(input, content...)
	return r.callWithInput(ctx, inst, inst.makeTextUpdate, input)
}

// EncodeStateVector returns the Yjs state vector for the document
// obtained by applying `update` to a fresh Y.Doc. Clients use the
// state vector to ask peers for the deltas they haven't observed
// yet; pairing it with the compact merged snapshot lets the
// snapshot endpoint deliver both the doc state and the
// catch-up watermark in a single response.
//
// An empty / nil update returns the state vector of a fresh
// (empty) doc, which is a one-byte encoding (the v1 zero-length
// SV).
func (r *YjsRuntime) EncodeStateVector(ctx context.Context, update []byte) ([]byte, error) {
	if r == nil {
		return nil, errors.New("collab: nil YjsRuntime")
	}
	inst, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer r.release(inst)
	return r.callWithInput(ctx, inst, inst.encodeStateVec, update)
}

// frameUpdates encodes a slice of update payloads into the
// length-prefixed concatenation format that ymerge's
// `merge_updates` expects (4-byte big-endian length per segment,
// followed by that many bytes of payload).
//
// Matches the OpaqueConcatFold output framing so a future code
// path could pipe an opaque-fold result straight back through
// the wasm merge with no re-framing — useful for migration when
// a folder flips from strict_zk to managed_encrypted.
func frameUpdates(updates [][]byte) []byte {
	total := 0
	for _, u := range updates {
		total += 4 + len(u)
	}
	out := make([]byte, 0, total)
	var lenBuf [4]byte
	for _, u := range updates {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(u)))
		out = append(out, lenBuf[:]...)
		out = append(out, u...)
	}
	return out
}
