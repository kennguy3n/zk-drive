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

// The NATS reconnect-backoff schedule now lives in internal/natsutil
// (shared by the server and worker); its unit tests moved there too
// (internal/natsutil/reconnect_test.go).
