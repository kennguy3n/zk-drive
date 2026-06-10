package notification

import (
	"context"
	"sync"

	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/typednil"

	"github.com/google/uuid"
)

// MobilePushSender delivers a notification to a user's registered native
// devices (iOS via APNs, Android via FCM). Implemented by
// *MobilePushService. Declared as an interface so the publisher depends
// on the behaviour, not the concrete type, keeping it unit-testable with
// a stub.
type MobilePushSender interface {
	Send(ctx context.Context, workspaceID, userID uuid.UUID, payload NotificationPayload) error
}

// MobilePushPublisher decorates an inner WSPublisher: it performs the
// normal publish (WebSocket / Web Push fan-out) and then, for a
// "notification" event, also delivers the same notification to the
// recipient's registered native devices via APNs / FCM.
//
// Unlike WebPushPublisher, mobile delivery is NOT gated on a live
// WebSocket connection: a user's phone is a distinct device from a
// browser tab, so they should receive the push on their phone even while
// a desktop tab is open. The OS coalesces redundant alerts by the
// per-notification collapse key (NotificationPayload.Tag).
//
// Wrap order at the call site is inner → WebPush → Mobile, so a single
// notification fans out across WebSocket, Web Push (offline browsers) and
// native push (phones) from one publish.
type MobilePushPublisher struct {
	inner WSPublisher
	push  MobilePushSender
	wg    *sync.WaitGroup
}

// NewMobilePushPublisher wraps inner so notifications also fan out to
// native devices via push. inner and push are normalised through
// typednil.IsTypedNil so a typed-nil concrete value wrapped in the
// interface engages the plain-nil path (consistent with the With*
// setters elsewhere in the codebase). A nil push degrades to plain
// inner-publish behaviour.
func NewMobilePushPublisher(inner WSPublisher, push MobilePushSender) *MobilePushPublisher {
	if typednil.IsTypedNil(inner) {
		inner = nil
	}
	if typednil.IsTypedNil(push) {
		push = nil
	}
	return &MobilePushPublisher{inner: inner, push: push}
}

// WithWaitGroup registers wg so each detached push goroutine is tracked
// (Add before launch, Done on return), letting graceful shutdown drain
// in-flight deliveries before the database pool closes — the same
// contract as WebPushPublisher.WithWaitGroup. The per-delivery
// pushDeliveryTimeout still bounds how long the drain can block.
func (p *MobilePushPublisher) WithWaitGroup(wg *sync.WaitGroup) *MobilePushPublisher {
	p.wg = wg
	return p
}

// Publish delegates to the inner publisher, then best-effort delivers a
// native push to the recipient's registered devices. The inner publish
// error (if any) is returned; push failures are swallowed by the
// service's own logging so a push outage never masks the WS result.
//
// Native push runs in a detached goroutine for the same reason as Web
// Push: delivery makes blocking HTTPS POSTs to APNs / FCM (hundreds of
// ms per device) and the synchronous WebSocket publish is what the HTTP
// handler actually waits on. The goroutine uses a cancellation-detached
// copy of ctx with its own timeout and still honours the request's
// logger / trace values.
func (p *MobilePushPublisher) Publish(ctx context.Context, workspaceID, userID uuid.UUID, event Event) error {
	var innerErr error
	if p.inner != nil {
		innerErr = p.inner.Publish(ctx, workspaceID, userID, event)
	}
	if p.push == nil {
		return innerErr
	}
	payload, ok := pushPayloadFromEvent(event)
	if !ok {
		return innerErr
	}
	pushCtx := context.WithoutCancel(ctx)
	// Add before launching (not inside the goroutine) so a concurrent Wait
	// at shutdown cannot observe the counter at zero between the `go`
	// statement and the goroutine starting — identical to WebPushPublisher.
	if p.wg != nil {
		p.wg.Add(1)
	}
	go func() {
		if p.wg != nil {
			defer p.wg.Done()
		}
		ctx, cancel := context.WithTimeout(pushCtx, pushDeliveryTimeout)
		defer cancel()
		if err := p.push.Send(ctx, workspaceID, userID, payload); err != nil {
			logging.FromContext(ctx).Error("notification mobile push failed",
				"workspace_id", workspaceID, "user_id", userID, "err", err)
		}
	}()
	return innerErr
}
