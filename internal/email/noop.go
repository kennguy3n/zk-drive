package email

import "context"

// NoopClient is the Sender used when no SMTP relay is configured.
// Every Send returns ErrNotConfigured so the metric / log layer can
// surface "disabled" outcomes distinct from real transport errors.
// IsConfigured returns false, which the boot code uses to emit a
// single startup warning rather than a per-send warning.
type NoopClient struct{}

// NewNoopClient is provided for symmetry with NewSMTPClient.
func NewNoopClient() *NoopClient { return &NoopClient{} }

// Send always returns ErrNotConfigured; the message is intentionally
// discarded. Callers MUST NOT use NoopClient in production.
func (NoopClient) Send(_ context.Context, _ Message) error {
	return ErrNotConfigured
}

// IsConfigured always returns false.
func (NoopClient) IsConfigured() bool { return false }
