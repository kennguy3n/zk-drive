package webhooks

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/kennguy3n/zk-drive/internal/tracing"
)

// SubjectEvents is the NATS JetStream subject every webhook event
// is published to. Kept as a package-level constant so the worker
// can subscribe to the same name without an import cycle. The
// "webhook." prefix matches the existing "drive." prefix for the
// preview / scan / index subjects, so the JetStream stream config
// in cmd/worker can use a single ">." catch-all if desired.
const SubjectEvents = "webhook.events"

// publisherTracerName is the OTel instrumentation-scope name. Same
// pattern as internal/jobs — lazy resolution via otel.Tracer per
// call so the no-op global provider that exists before tracing.Init
// doesn't get cached.
const publisherTracerName = "github.com/kennguy3n/zk-drive/internal/webhooks"

// Publisher emits events onto the JetStream subject. A nil
// *Publisher is a valid no-op receiver — API handlers wire the
// pointer once at startup and can call methods unconditionally
// without nil-checking, the same contract internal/jobs uses.
type Publisher struct {
	js nats.JetStreamContext
}

// NewPublisher constructs a Publisher bound to the given JetStream
// context. Pass nil to disable publishing (every Publish call becomes
// a no-op returning nil).
func NewPublisher(js nats.JetStreamContext) *Publisher {
	if js == nil {
		return nil
	}
	return &Publisher{js: js}
}

// Publish serialises an Event and emits it on SubjectEvents. The
// W3C trace-context is injected onto the NATS message headers so the
// consumer (the delivery worker) reconstructs the parent-child
// relationship across the queue boundary.
//
// Safe to call on a nil receiver.
func (p *Publisher) Publish(ctx context.Context, ev Event) error {
	if p == nil || p.js == nil {
		return nil
	}
	ctx, span := otel.Tracer(publisherTracerName).Start(ctx,
		"jetstream.publish "+SubjectEvents,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemKey.String("nats"),
			semconv.MessagingDestinationName(SubjectEvents),
			semconv.MessagingOperationTypePublish,
			attribute.String("webhooks.event.id", ev.ID.String()),
			attribute.String("webhooks.event.type", string(ev.Type)),
			attribute.String("webhooks.workspace_id", ev.WorkspaceID.String()),
		),
	)
	defer span.End()

	body, err := json.Marshal(ev)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal event")
		return fmt.Errorf("marshal webhook event: %w", err)
	}
	// No nats.Context option is passed to the async publish: JetStream
	// rejects a per-call context when the JetStream context already
	// carries a default request timeout ("context and timeout can not
	// both be set"). Trace context propagates via the headers above.
	msg := &nats.Msg{Subject: SubjectEvents, Data: body, Header: nats.Header{}}
	otel.GetTextMapPropagator().Inject(ctx, tracing.NATSHeaderCarrier(msg.Header))
	if _, err := p.js.PublishMsgAsync(msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish failed")
		return fmt.Errorf("publish webhook event: %w", err)
	}
	return nil
}

// PublishFileEvent is a convenience helper for the "file.*" namespace.
// Marshals the FileEventData payload and emits the envelope.
func (p *Publisher) PublishFileEvent(ctx context.Context, t EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data FileEventData) error {
	if p == nil {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal file event data: %w", err)
	}
	return p.Publish(ctx, NewEvent(t, workspaceID, actorID, raw))
}

// PublishPermissionEvent is the equivalent helper for permission.*
// events.
func (p *Publisher) PublishPermissionEvent(ctx context.Context, t EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data PermissionEventData) error {
	if p == nil {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal permission event data: %w", err)
	}
	return p.Publish(ctx, NewEvent(t, workspaceID, actorID, raw))
}

// PublishMemberEvent is the equivalent helper for member.* events.
func (p *Publisher) PublishMemberEvent(ctx context.Context, t EventType, workspaceID uuid.UUID, actorID *uuid.UUID, data MemberEventData) error {
	if p == nil {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal member event data: %w", err)
	}
	return p.Publish(ctx, NewEvent(t, workspaceID, actorID, raw))
}
