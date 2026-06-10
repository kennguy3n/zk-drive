package preview

import (
	"context"
	"sync/atomic"
)

// subprocessGate bounds the number of heavy (subprocess) renders that
// may run concurrently in a single worker process. LibreOffice, in
// particular, is single-threaded and memory-hungry: each `soffice`
// conversion can hold hundreds of MB resident, so an unbounded fan-out
// of office jobs on one pod OOM-kills the worker. ffmpeg, ImageMagick,
// pdftoppm and rsvg-convert have the same shape to a lesser degree.
//
// The gate is a counting semaphore implemented as a buffered channel.
// A nil gate means "unlimited" — the default — so deployments that do
// not set PreviewWorkerConcurrency behave exactly as before.
//
// The gate is stored behind an atomic.Pointer so SetSubprocessConcurrency
// can install it at worker startup without locking the hot path; after
// startup the pointer is read-only and Load is a single atomic load.
type subprocessGate struct {
	slots chan struct{}
}

var subprocessGatePtr atomic.Pointer[subprocessGate]

// SetSubprocessConcurrency caps concurrent heavy (subprocess) renders
// in this process at n. n <= 0 removes any cap (unlimited), which is
// the default. It is intended to be called once at worker startup,
// before any preview job is dispatched.
func SetSubprocessConcurrency(n int) {
	if n <= 0 {
		subprocessGatePtr.Store(nil)
		return
	}
	subprocessGatePtr.Store(&subprocessGate{slots: make(chan struct{}, n)})
}

// SubprocessConcurrency reports the configured cap, or 0 when no cap is
// set (unlimited). Exposed for startup logging / observability.
func SubprocessConcurrency() int {
	g := subprocessGatePtr.Load()
	if g == nil {
		return 0
	}
	return cap(g.slots)
}

// acquireSubprocessSlot blocks until a heavy-render slot is free or ctx
// is cancelled. It returns a release function that MUST be called once
// the render completes (typically via defer). When no cap is configured
// the returned release is a no-op and the call never blocks.
//
// The slot is acquired ONCE per heavy preview job (in Service.Generate),
// never per individual subprocess. This is deliberate: a single office
// render shells out to LibreOffice and then pdftoppm in sequence, so
// gating each exec separately would let one job hold two slots — and
// with a cap of 1 it would deadlock waiting on itself. One slot per job
// keeps the accounting honest and matches what the cap is meant to
// bound: concurrent heavy pipelines, not raw process spawns.
func acquireSubprocessSlot(ctx context.Context) (func(), error) {
	g := subprocessGatePtr.Load()
	if g == nil {
		return func() {}, nil
	}
	select {
	case g.slots <- struct{}{}:
		var once atomic.Bool
		return func() {
			if once.CompareAndSwap(false, true) {
				<-g.slots
			}
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
