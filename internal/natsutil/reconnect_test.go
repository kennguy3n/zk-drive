package natsutil

import (
	"testing"
	"time"
)

// TestReconnectDelay pins the WS8 8.4 exponential-backoff schedule: the
// pre-jitter base delay doubles each attempt from ReconnectBaseDelay and
// clamps at ReconnectMaxDelay, so a brief blip recovers fast while a
// prolonged outage settles into a low-frequency retry.
func TestReconnectDelay(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{-1, ReconnectBaseDelay}, // defensive: <1 treated as 1
		{0, ReconnectBaseDelay},
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, ReconnectMaxDelay}, // 32s would exceed the 30s cap
		{7, ReconnectMaxDelay},
		{100, ReconnectMaxDelay},
	}
	for _, c := range cases {
		if got := ReconnectDelay(c.attempts); got != c.want {
			t.Errorf("ReconnectDelay(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// TestReconnectDelayMonotonic guards the invariant that the backoff
// never decreases and never exceeds the cap, regardless of attempt count.
func TestReconnectDelayMonotonic(t *testing.T) {
	prev := time.Duration(0)
	for a := 1; a <= 50; a++ {
		d := ReconnectDelay(a)
		if d < prev {
			t.Fatalf("backoff decreased at attempt %d: %v < %v", a, d, prev)
		}
		if d > ReconnectMaxDelay {
			t.Fatalf("backoff exceeded cap at attempt %d: %v", a, d)
		}
		prev = d
	}
}
