package preview

import "sync/atomic"

// binaryVar holds the name (or absolute path) of an external preview
// binary. Reads from worker goroutines happen on every render
// (concurrent, hot path); writes happen at worker startup via the
// public Set* setters or, in tests, via withBinarySwap. Without
// synchronisation those concurrent reads + writes would be a data
// race that the race detector catches and that the Go memory model
// does not allow.
//
// atomic.Value gives lock-free reads (a single Load + interface
// type-assertion) which is meaningfully cheaper than wrapping every
// renderer call site in an RWMutex.RLock — the lookup is on the hot
// path for every preview job. Stores are atomic too, so the test
// helper withBinarySwap (which serialises stores across parallel
// tests for cleanup atomicity) can safely run alongside renderer
// goroutines.
type binaryVar struct {
	v atomic.Value // string; never reads as zero after newBinaryVar
}

// newBinaryVar returns a binaryVar pre-seeded with the supplied
// initial command/path. Use this in package-level vars; the seed
// runs at init() time before any worker goroutines, so the first
// renderer Load() is guaranteed to see a non-zero value.
func newBinaryVar(initial string) *binaryVar {
	b := &binaryVar{}
	b.v.Store(initial)
	return b
}

// Get returns the currently configured binary name/path. Safe for
// concurrent use with Set.
func (b *binaryVar) Get() string {
	// atomic.Value seeded by newBinaryVar always holds a string, so
	// the assertion cannot fail. Defensive zero-value return covers
	// the impossible-but-explicit "Load before Store" branch.
	s, _ := b.v.Load().(string)
	return s
}

// Set updates the binary name/path. Safe for concurrent use with
// Get. Production callers (e.g. SetImageMagickBinary) and tests
// should both go through this rather than touching the underlying
// atomic.Value directly.
func (b *binaryVar) Set(name string) {
	b.v.Store(name)
}
