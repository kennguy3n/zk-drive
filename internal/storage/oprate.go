package storage

import (
	"sync"
	"time"
)

// opRateWindow is the trailing window over which the storage client
// summarises its operation error rate for the admin health dashboard
// (WS8). Five minutes is long enough that a low-traffic deployment
// still has a meaningful denominator, yet short enough that a
// recovered gateway clears the signal promptly.
const opRateWindow = 5 * time.Minute

// opRateResolution is the bucket width of the ring buffer. One-second
// buckets keep the window summary cheap (a fixed 300-entry ring) and
// precise enough — the dashboard reports a rate, not millisecond
// timing.
const opRateResolution = time.Second

// opRateBuckets is the fixed number of buckets in the ring.
const opRateBuckets = int(opRateWindow / opRateResolution)

// opBucket accumulates the operation counts for a single
// opRateResolution-wide slice of time. sec is the Unix-second the
// bucket currently represents; when a record arrives for a newer
// second that maps to the same ring slot, the slot is reset to the
// new second first (the old contents have aged out of any window that
// could still reference them, because the ring spans exactly the
// window).
type opBucket struct {
	sec    int64
	total  uint64
	errors uint64
}

// opRate is a concurrency-safe, fixed-memory rolling counter of
// storage operations and how many of them failed. It is intentionally
// lock-based rather than lock-free: the recorded operations are
// server-side direct S3 calls (audit-archive writes, restore reads,
// health checks), not the per-request presign hot path, so contention
// is negligible and a mutex keeps the ring logic obviously correct.
type opRate struct {
	mu      sync.Mutex
	buckets [opRateBuckets]opBucket
	now     func() time.Time // injectable for tests; defaults to time.Now
}

func newOpRate() *opRate {
	return &opRate{now: time.Now}
}

// record bumps the current bucket's total and, when failed, its error
// count.
func (r *opRate) record(failed bool) {
	t := r.now()
	sec := t.Unix()
	idx := int(sec % int64(opRateBuckets))

	r.mu.Lock()
	defer r.mu.Unlock()
	b := &r.buckets[idx]
	if b.sec != sec {
		// Ring slot is being reused for a new second: reset it.
		b.sec = sec
		b.total = 0
		b.errors = 0
	}
	b.total++
	if failed {
		b.errors++
	}
}

// OpStats is the trailing-window summary the health dashboard renders.
type OpStats struct {
	// Total is the number of recorded operations in the window.
	Total uint64
	// Errors is how many of those operations failed.
	Errors uint64
	// Window is the trailing duration the summary covers.
	Window time.Duration
}

// ErrorRate returns Errors/Total in [0,1], or 0 when there were no
// operations (an idle client is not an erroring client).
func (s OpStats) ErrorRate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Errors) / float64(s.Total)
}

// stats sums every bucket whose second falls within the trailing
// window.
func (r *opRate) stats() OpStats {
	cutoff := r.now().Add(-opRateWindow).Unix()
	var total, errs uint64

	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.buckets {
		b := r.buckets[i]
		if b.sec >= cutoff {
			total += b.total
			errs += b.errors
		}
	}
	return OpStats{Total: total, Errors: errs, Window: opRateWindow}
}
