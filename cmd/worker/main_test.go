package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestStartJobPool_DrainWaitsForInFlight verifies the contract relied
// on by run()'s shutdown sequence: drain() must block until a handler
// that is mid-execution when shutdown begins has fully returned (its
// msg.Ack would land here), not merely until the message was dequeued.
func TestStartJobPool_DrainWaitsForInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bg sync.WaitGroup
	started := make(chan struct{})
	release := make(chan struct{})
	var completed atomic.Int32

	h := func(*nats.Msg) {
		close(started)
		<-release
		completed.Add(1)
	}
	handler, drain := startJobPool(ctx, &bg, 1, h)

	go handler(&nats.Msg{Subject: "test"})
	<-started // worker is now inside h, mid-flight

	drainReturned := make(chan struct{})
	go func() {
		drain()
		close(drainReturned)
	}()

	// drain must NOT return while h is still running.
	select {
	case <-drainReturned:
		t.Fatal("drain returned before in-flight handler completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(release) // let h finish

	select {
	case <-drainReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return after in-flight handler completed")
	}
	if got := completed.Load(); got != 1 {
		t.Fatalf("handler completion count = %d, want 1", got)
	}
	bg.Wait() // pool goroutine exited
}

// TestStartJobPool_DrainUnblocksSaturatedSend verifies drain does not
// panic (send on closed channel) or deadlock when a NATS callback is
// blocked on the unbuffered hand-off because every worker is busy. The
// blocked send must unblock via the stop signal and the message is
// simply left for redelivery.
func TestStartJobPool_DrainUnblocksSaturatedSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bg sync.WaitGroup
	started := make(chan struct{})
	release := make(chan struct{})
	var completed atomic.Int32

	h := func(*nats.Msg) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		completed.Add(1)
	}
	handler, drain := startJobPool(ctx, &bg, 1, h)

	go handler(&nats.Msg{Subject: "busy"}) // occupies the sole worker
	<-started

	// Second send blocks: the worker is busy and the channel is
	// unbuffered. It must return (not panic) once drain fires.
	secondReturned := make(chan struct{})
	go func() {
		handler(&nats.Msg{Subject: "queued"})
		close(secondReturned)
	}()

	drainReturned := make(chan struct{})
	go func() {
		drain()
		close(drainReturned)
	}()

	select {
	case <-secondReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("saturated send did not unblock after drain")
	}

	close(release)

	select {
	case <-drainReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return")
	}
	// Only the first message ran; the queued one was dropped for
	// redelivery rather than processed after stop.
	if got := completed.Load(); got != 1 {
		t.Fatalf("handler completion count = %d, want 1", got)
	}
	bg.Wait()
}

// TestStartJobPool_DrainIdempotent ensures calling drain more than once
// (defensive: defer plus an explicit call) does not panic on a double
// close of the stop channel.
func TestStartJobPool_DrainIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bg sync.WaitGroup
	_, drain := startJobPool(ctx, &bg, 2, func(*nats.Msg) {})
	drain()
	drain()
	bg.Wait()
}

// TestNatsReconnectDelay pins the WS8 8.4 exponential-backoff schedule:
// the pre-jitter base delay doubles each attempt from natsReconnectBaseDelay
// and clamps at natsReconnectMaxDelay, so a brief blip recovers fast while
// a prolonged outage settles into a low-frequency retry.
func TestNatsReconnectDelay(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{-1, natsReconnectBaseDelay}, // defensive: <1 treated as 1
		{0, natsReconnectBaseDelay},
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, natsReconnectMaxDelay}, // 32s would exceed the 30s cap
		{7, natsReconnectMaxDelay},
		{100, natsReconnectMaxDelay},
	}
	for _, c := range cases {
		if got := natsReconnectDelay(c.attempts); got != c.want {
			t.Errorf("natsReconnectDelay(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// TestNatsReconnectDelayMonotonic guards the invariant that the backoff
// never decreases and never exceeds the cap, regardless of attempt count.
func TestNatsReconnectDelayMonotonic(t *testing.T) {
	prev := time.Duration(0)
	for a := 1; a <= 50; a++ {
		d := natsReconnectDelay(a)
		if d < prev {
			t.Fatalf("backoff decreased at attempt %d: %v < %v", a, d, prev)
		}
		if d > natsReconnectMaxDelay {
			t.Fatalf("backoff exceeded cap at attempt %d: %v", a, d)
		}
		prev = d
	}
}
