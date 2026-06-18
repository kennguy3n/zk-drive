// Package natsutil holds the NATS reconnect policy shared by the
// server and worker binaries (auto-healing). Both connect to
// the same JetStream and must back off identically during a shared
// outage; keeping the schedule in one place makes that consistency a
// compile-time fact instead of a comment asking two files to be kept
// in sync by hand.
package natsutil

import "time"

const (
	// ReconnectBaseDelay / ReconnectMaxDelay bound the exponential
	// reconnect backoff: the delay doubles each attempt from the base
	// up to the cap, so a brief blip recovers in ~1s while a prolonged
	// outage settles into a low-frequency 30s retry instead of a hot
	// reconnect loop.
	ReconnectBaseDelay = 1 * time.Second
	ReconnectMaxDelay  = 30 * time.Second
	// ReconnectJitter is the +/- randomisation applied to each reconnect
	// delay so a fleet of server+worker processes does not reconnect in
	// lockstep (thundering herd) after a shared NATS outage.
	ReconnectJitter = 1 * time.Second
)

// ReconnectDelay is the nats.CustomReconnectDelay backoff: exponential
// from ReconnectBaseDelay, doubling each attempt, clamped at
// ReconnectMaxDelay. nats.go adds the configured jitter on top, so the
// returned value is the pre-jitter base delay.
func ReconnectDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := ReconnectBaseDelay
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= ReconnectMaxDelay {
			return ReconnectMaxDelay
		}
	}
	return delay
}
