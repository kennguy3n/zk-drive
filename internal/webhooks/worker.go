package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// MetricsRecorder is the bounded surface the delivery worker needs
// from the metrics package. Defined here so internal/webhooks does
// NOT depend on internal/metrics directly (cmd/server wires both, so
// a direct import would create a cycle when shared types move
// either way). Production callers pass a thin adapter.
type MetricsRecorder interface {
	RecordWebhookDelivery(outcome string, statusCode int, duration time.Duration)
}

// DeliveryWorker is the JetStream consumer that turns published
// events into HTTP POSTs against subscriber URLs. One instance lives
// in cmd/worker; calling Consume processes a single delivery and
// returns a job result (ok / skip / error / dropped) that cmd/worker
// translates into an Ack/Nak.
type DeliveryWorker struct {
	repo    Repository
	client  *DeliveryClient
	metrics MetricsRecorder
	now     func() time.Time
}

// NewDeliveryWorker constructs a DeliveryWorker. The repo and client
// dependencies are required; passing a nil metrics recorder turns
// metric emission into no-ops (useful for tests).
func NewDeliveryWorker(repo Repository, client *DeliveryClient, metrics MetricsRecorder) (*DeliveryWorker, error) {
	if repo == nil {
		return nil, errors.New("webhooks: repository is required")
	}
	if client == nil {
		return nil, errors.New("webhooks: delivery client is required")
	}
	return &DeliveryWorker{
		repo:    repo,
		client:  client,
		metrics: metrics,
		now:     time.Now,
	}, nil
}

// Consume processes one JetStream message carrying a webhook Event.
// Returns a job-result string ("ok" / "skip" / "error" / "dropped")
// that cmd/worker's tracing.WrapConsumer signature expects. The
// JetStream loop in cmd/worker translates these into Ack / Nak with
// delay / Term so JetStream's redelivery scheduler does the retry
// timing for us.
//
// The consumer fan-out is per-event: one message in -> N HTTP calls
// out (one per subscription matching event_type for the workspace).
// We persist the per-attempt webhook_deliveries row BEFORE returning
// so an operator's "show recent deliveries" query always sees the
// activity even if the worker dies mid-fan-out.
func (w *DeliveryWorker) Consume(ctx context.Context, msg *nats.Msg) string {
	if msg == nil || len(msg.Data) == 0 {
		slog.WarnContext(ctx, "webhooks worker received empty message")
		return "dropped"
	}
	var ev Event
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		slog.ErrorContext(ctx, "webhooks worker failed to unmarshal event",
			"err", err, "data_len", len(msg.Data))
		return "dropped"
	}
	if ev.WorkspaceID == uuid.Nil || ev.Type == "" {
		slog.WarnContext(ctx, "webhooks worker received malformed event",
			"event_id", ev.ID, "workspace_id", ev.WorkspaceID, "event_type", ev.Type)
		return "dropped"
	}

	subs, err := w.repo.ListActiveForEvent(ctx, ev.WorkspaceID, ev.Type)
	if err != nil {
		slog.ErrorContext(ctx, "webhooks worker failed to list subscriptions",
			"err", err, "workspace_id", ev.WorkspaceID, "event_type", ev.Type)
		return "error"
	}
	if len(subs) == 0 {
		// No subscribers for this (workspace, event_type) — the
		// publisher fans out by workspace, not by subscription,
		// so this is the common case for events that have no
		// matching consumers (the vast majority of events).
		// Ack silently.
		return "skip"
	}

	body, err := json.Marshal(ev)
	if err != nil {
		// Should never happen — the publisher already
		// marshalled successfully — but record the failure
		// rather than silently drop.
		slog.ErrorContext(ctx, "webhooks worker failed to remarshal event",
			"err", err, "event_id", ev.ID)
		return "error"
	}

	// MsgMetaData carries the JetStream delivery attempt counter
	// (1 for first delivery, 2+ for redelivery). We use this as
	// AttemptNumber on the webhook_deliveries row AND to compute
	// the next-retry backoff for Nak.
	attempt := 1
	if md, mdErr := msg.Metadata(); mdErr == nil && md != nil {
		attempt = int(md.NumDelivered)
		if attempt < 1 {
			attempt = 1
		}
	}

	terminal := attempt >= MaxAttempts
	allSucceeded := true
	for _, s := range subs {
		// Extend the JetStream ack deadline before each
		// per-subscription delivery so the worst-case fan-out
		// (MaxSubscriptionsPerWorkspace × DefaultDeliveryTimeout
		// = 20 × 30s = 10min) can never exceed the consumer's
		// AckWait window (configured at 5min in cmd/worker).
		// Without InProgress() JetStream would assume the worker
		// died mid-fan-out and redeliver the message, causing a
		// thundering-herd of duplicates against subscribers that
		// already received the event. InProgress is documented to
		// reset the deadline by AckWait on each call.
		_ = msg.InProgress()
		ok := w.deliverOne(ctx, &ev, s, body, attempt, terminal)
		if !ok {
			allSucceeded = false
		}
	}
	if allSucceeded {
		return "ok"
	}
	// Returning "error" tells cmd/worker to Nak so JetStream
	// re-queues the message for redelivery. The redelivery cadence
	// is configured on the consumer (AckWait + BackOff schedule);
	// we additionally publish the next_retry_at timestamp on the
	// stored Delivery row so the admin UI can show "next attempt
	// in 3m 12s" without having to peek at JetStream internals.
	if terminal {
		// Final attempt failed — Term the message so JetStream
		// doesn't keep redelivering forever. The "dropped"
		// result string maps to msg.Term() in cmd/worker.
		return "dropped"
	}
	return "error"
}

// deliverOne handles delivery to a single subscription. Returns true
// on success (2xx), false otherwise. Always records a
// webhook_deliveries row and updates the subscription counters before
// returning.
func (w *DeliveryWorker) deliverOne(ctx context.Context, ev *Event, sub *Subscription, body []byte, attempt int, terminal bool) bool {
	u, parseErr := url.Parse(sub.URL)
	if parseErr != nil {
		// A stored URL that no longer parses is an "impossible"
		// state — the create-time validation would have rejected
		// it. Record the failure so the admin can see what
		// happened, then bail.
		w.recordFailure(ctx, ev, sub, attempt, OutcomeBlocked, 0, "", fmt.Sprintf("stored URL no longer parses: %v", parseErr), 0, terminal)
		return false
	}
	signer, sErr := NewSigner(sub.Secret)
	if sErr != nil {
		w.recordFailure(ctx, ev, sub, attempt, OutcomeBlocked, 0, "", fmt.Sprintf("subscription secret invalid: %v", sErr), 0, terminal)
		return false
	}

	deliveryID := uuid.New()
	ts := w.now()
	res := w.client.Deliver(ctx, u, ev.ID, deliveryID, ev.Type, body, signer, ts)

	durMs := int(res.Duration / time.Millisecond)
	d := Delivery{
		ID:             deliveryID,
		SubscriptionID: sub.ID,
		WorkspaceID:    sub.WorkspaceID,
		EventID:        ev.ID,
		EventType:      ev.Type,
		AttemptNumber:  attempt,
		Outcome:        res.Outcome,
		StatusCode:     res.StatusCode,
		ResponseBody:   res.ResponseBody,
		ErrorMessage:   res.ErrorMessage,
		DurationMs:     durMs,
		AttemptedAt:    ts.UTC(),
	}
	if res.Outcome != OutcomeSuccess && !terminal {
		nr := ts.Add(BackoffDelay(attempt + 1)).UTC()
		d.NextRetryAt = &nr
	}
	if err := w.repo.InsertDelivery(ctx, &d); err != nil {
		slog.ErrorContext(ctx, "webhooks worker failed to insert delivery",
			"err", err, "subscription_id", sub.ID, "event_id", ev.ID)
	}
	if err := w.repo.UpdateAttempt(ctx, sub.WorkspaceID, sub.ID, res.Outcome, ts); err != nil {
		slog.ErrorContext(ctx, "webhooks worker failed to update subscription counters",
			"err", err, "subscription_id", sub.ID)
	}
	if w.metrics != nil {
		w.metrics.RecordWebhookDelivery(string(res.Outcome), res.StatusCode, res.Duration)
	}
	return res.Outcome == OutcomeSuccess
}

// recordFailure is the helper for the rare path where deliveryOne
// can't even attempt the HTTP request (parse / secret issues). Writes
// the webhook_deliveries row directly without going through the
// HTTP client.
func (w *DeliveryWorker) recordFailure(
	ctx context.Context, ev *Event, sub *Subscription,
	attempt int, outcome DeliveryOutcome, statusCode int,
	responseBody, errorMessage string, duration time.Duration,
	terminal bool,
) {
	ts := w.now()
	d := Delivery{
		ID:             uuid.New(),
		SubscriptionID: sub.ID,
		WorkspaceID:    sub.WorkspaceID,
		EventID:        ev.ID,
		EventType:      ev.Type,
		AttemptNumber:  attempt,
		Outcome:        outcome,
		StatusCode:     statusCode,
		ResponseBody:   responseBody,
		ErrorMessage:   errorMessage,
		DurationMs:     int(duration / time.Millisecond),
		AttemptedAt:    ts.UTC(),
	}
	if outcome != OutcomeSuccess && !terminal {
		nr := ts.Add(BackoffDelay(attempt + 1)).UTC()
		d.NextRetryAt = &nr
	}
	if err := w.repo.InsertDelivery(ctx, &d); err != nil {
		slog.ErrorContext(ctx, "webhooks worker failed to insert delivery (pre-send failure)",
			"err", err, "subscription_id", sub.ID)
	}
	if err := w.repo.UpdateAttempt(ctx, sub.WorkspaceID, sub.ID, outcome, ts); err != nil {
		slog.ErrorContext(ctx, "webhooks worker failed to update subscription counters (pre-send failure)",
			"err", err, "subscription_id", sub.ID)
	}
	if w.metrics != nil {
		w.metrics.RecordWebhookDelivery(string(outcome), statusCode, duration)
	}
}
