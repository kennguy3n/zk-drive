package storage

import (
	"testing"
	"time"
)

func TestOpRateEmptyIsZero(t *testing.T) {
	r := newOpRate()
	s := r.stats()
	if s.Total != 0 || s.Errors != 0 {
		t.Fatalf("empty op rate should be zero, got %+v", s)
	}
	if s.ErrorRate() != 0 {
		t.Fatalf("idle client error rate should be 0, got %v", s.ErrorRate())
	}
	if s.Window != opRateWindow {
		t.Fatalf("window = %v, want %v", s.Window, opRateWindow)
	}
}

func TestOpRateCountsWithinWindow(t *testing.T) {
	clock := &orClock{t: time.Unix(1_700_000_000, 0)}
	r := newOpRate()
	r.now = clock.now

	// 8 ops, 2 failed, all in the same second.
	for i := 0; i < 6; i++ {
		r.record(false)
	}
	r.record(true)
	r.record(true)

	s := r.stats()
	if s.Total != 8 || s.Errors != 2 {
		t.Fatalf("got total=%d errors=%d, want 8/2", s.Total, s.Errors)
	}
	if got := s.ErrorRate(); got != 0.25 {
		t.Fatalf("error rate = %v, want 0.25", got)
	}
}

func TestOpRateEvictsOutsideWindow(t *testing.T) {
	clock := &orClock{t: time.Unix(1_700_000_000, 0)}
	r := newOpRate()
	r.now = clock.now

	// Record a failure now.
	r.record(true)
	// Advance beyond the full window so the bucket ages out.
	clock.advance(opRateWindow + 2*time.Second)
	// A fresh success in the new second.
	r.record(false)

	s := r.stats()
	if s.Total != 1 || s.Errors != 0 {
		t.Fatalf("stale bucket not evicted: total=%d errors=%d, want 1/0", s.Total, s.Errors)
	}
}

func TestOpRateRingSlotReuseResets(t *testing.T) {
	clock := &orClock{t: time.Unix(1_700_000_000, 0)}
	r := newOpRate()
	r.now = clock.now

	r.record(true)
	// Advance exactly opRateBuckets seconds so we wrap to the same
	// ring slot; the old contents must be reset, not accumulated.
	clock.advance(time.Duration(opRateBuckets) * time.Second)
	r.record(false)

	s := r.stats()
	if s.Total != 1 || s.Errors != 0 {
		t.Fatalf("ring slot not reset on reuse: total=%d errors=%d, want 1/0", s.Total, s.Errors)
	}
}

type orClock struct {
	t time.Time
}

func (c *orClock) now() time.Time          { return c.t }
func (c *orClock) advance(d time.Duration) { c.t = c.t.Add(d) }
