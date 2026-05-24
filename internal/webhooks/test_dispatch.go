package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// TestDispatcher delivers a synthetic event SYNCHRONOUSLY to a
// single, explicitly-targeted subscription. It is the back-end of
// POST /api/admin/webhooks/{id}/test in api/webhooks.
//
// Why a separate component (vs. reusing the JetStream publisher):
//
//   - The publisher fans out to EVERY active subscription matching
//     (workspace_id, event_type). If an admin has three subscriptions
//     for file.upload.confirmed and tests subscription A, publishing
//     a synthetic event would also deliver to subscriptions B and C.
//     Production subscribers may not branch on `{"test": true}` and
//     would process the synthetic event as if it were real.
//
//   - A synchronous test gives the admin immediate inline feedback
//     ("HTTP 200, 47ms") in the UI rather than "enqueued — refresh
//     deliveries to see the outcome". This is the Stripe / GitHub
//     Apps pattern for "Send test event" admin UX.
//
//   - Test deliveries deliberately do NOT update the subscription's
//     consecutive_failures counter (an admin debugging a broken URL
//     should not accidentally auto-pause the subscription); the
//     webhook_deliveries row IS recorded so the operator can see
//     the attempt in the history view, but UpdateAttempt is skipped.
//     A delivery row from a test is distinguishable in the history
//     by its EventType payload carrying {"test": true} and by the
//     attempt_number=1 / NextRetryAt=nil shape.
type TestDispatcher struct {
	repo   Repository
	client *DeliveryClient
	now    func() time.Time
}

// NewTestDispatcher constructs a TestDispatcher. The repository and
// delivery client are required (the dispatcher cannot persist the
// outcome without repo nor make the HTTP call without client).
func NewTestDispatcher(repo Repository, client *DeliveryClient) (*TestDispatcher, error) {
	if repo == nil {
		return nil, errors.New("webhooks: repository is required")
	}
	if client == nil {
		return nil, errors.New("webhooks: delivery client is required")
	}
	return &TestDispatcher{repo: repo, client: client, now: time.Now}, nil
}

// Dispatch performs one synchronous delivery to sub. The caller
// supplies the event_type the synthetic event should carry (almost
// always sub.EventType, but kept as a parameter so the handler can
// emit a wider check in the future). Returns the resulting Delivery
// row, OR an error if the URL stored on the subscription is so
// broken that no HTTP call could even be attempted (the row is still
// persisted so the operator can see the failure shape; we return the
// row plus a non-nil error to the caller).
//
// Auto-pause behaviour: NOT triggered by this path. See the
// TestDispatcher docstring for the reasoning.
func (d *TestDispatcher) Dispatch(ctx context.Context, sub *Subscription) (*Delivery, error) {
	if sub == nil {
		return nil, errors.New("webhooks: subscription is required")
	}
	if !sub.Active {
		// Allow testing a paused subscription — that's the whole
		// point: an admin who paused it after a string of failures
		// wants to verify the endpoint is now reachable before
		// resuming. We do NOT short-circuit here.
		_ = sub.Active
	}
	u, parseErr := url.Parse(sub.URL)
	if parseErr != nil {
		row := d.recordBlocked(ctx, sub, fmt.Sprintf("stored URL no longer parses: %v", parseErr))
		return row, fmt.Errorf("subscription URL invalid: %w", parseErr)
	}
	signer, sErr := NewSigner(sub.Secret)
	if sErr != nil {
		row := d.recordBlocked(ctx, sub, fmt.Sprintf("subscription secret invalid: %v", sErr))
		return row, fmt.Errorf("subscription secret invalid: %w", sErr)
	}
	// Build the synthetic event. The payload carries an explicit
	// "test": true marker AND the target subscription's id so the
	// subscriber (and any in-flight log scraper) can see this was an
	// admin-triggered probe rather than an organic event. The
	// envelope uses the subscription's event_type so subscribers
	// that branch on Type don't have to learn a special "test" type.
	payload, _ := json.Marshal(map[string]any{
		"test":            true,
		"subscription_id": sub.ID,
	})
	ev := NewEvent(sub.EventType, sub.WorkspaceID, nil, payload)
	body, err := json.Marshal(ev)
	if err != nil {
		// json.Marshal can only fail on cyclic structures or
		// unsupported types; neither applies to our envelope.
		row := d.recordBlocked(ctx, sub, fmt.Sprintf("marshal test event: %v", err))
		return row, fmt.Errorf("marshal test event: %w", err)
	}

	deliveryID := uuid.New()
	ts := d.now()
	res := d.client.Deliver(ctx, u, ev.ID, deliveryID, ev.Type, body, signer, ts)
	dur := int(res.Duration / time.Millisecond)
	row := &Delivery{
		ID:             deliveryID,
		SubscriptionID: sub.ID,
		WorkspaceID:    sub.WorkspaceID,
		EventID:        ev.ID,
		EventType:      ev.Type,
		// Tests are always attempt 1 and never retried (terminal=true);
		// NextRetryAt stays nil even on failure.
		AttemptNumber: 1,
		Outcome:       res.Outcome,
		StatusCode:    res.StatusCode,
		ResponseBody:  res.ResponseBody,
		ErrorMessage:  res.ErrorMessage,
		DurationMs:    dur,
		AttemptedAt:   ts.UTC(),
	}
	// Persist the row so an admin can see it in
	// /webhooks/{id}/deliveries. We DO NOT call
	// repo.UpdateAttempt here: a synchronous test must not move
	// consecutive_failures (or auto-pause the subscription).
	if err := d.repo.InsertDelivery(ctx, row); err != nil {
		return row, fmt.Errorf("persist delivery: %w", err)
	}
	return row, nil
}

// recordBlocked writes an OutcomeBlocked delivery row for the rare
// pre-send failures (bad URL parse, bad signer construction). Mirrors
// DeliveryWorker.recordFailure but explicitly never updates the
// subscription counters — see TestDispatcher docs.
func (d *TestDispatcher) recordBlocked(ctx context.Context, sub *Subscription, msg string) *Delivery {
	ts := d.now()
	row := &Delivery{
		ID:             uuid.New(),
		SubscriptionID: sub.ID,
		WorkspaceID:    sub.WorkspaceID,
		EventID:        uuid.New(),
		EventType:      sub.EventType,
		AttemptNumber:  1,
		Outcome:        OutcomeBlocked,
		ErrorMessage:   msg,
		AttemptedAt:    ts.UTC(),
	}
	_ = d.repo.InsertDelivery(ctx, row)
	return row
}
