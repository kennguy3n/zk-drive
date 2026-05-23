package tracing

import (
	"context"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// NATSHeaderCarrier adapts nats.Header to the OpenTelemetry
// propagation.TextMapCarrier interface so the global propagator can
// inject or extract W3C trace-context (traceparent / tracestate) and
// baggage onto/from NATS message headers. Exported so both the
// publisher (internal/jobs) and consumer (this package) share one
// canonical implementation — avoids the duplication footgun where a
// future key-normalisation fix would need to land in two places.
type NATSHeaderCarrier nats.Header

func (c NATSHeaderCarrier) Get(key string) string  { return nats.Header(c).Get(key) }
func (c NATSHeaderCarrier) Set(key, value string)  { nats.Header(c).Set(key, value) }
func (c NATSHeaderCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// consumerTracerName is the instrumentation-scope name. We resolve
// the tracer lazily via otel.Tracer on each call rather than
// caching at init: a cached tracer would bind to the no-op global
// provider that exists before tracing.Init runs, and any later
// SetTracerProvider call (including the one inside Init) would not
// reach the cached value. Costs us one map lookup per call which is
// negligible vs network IO.
const consumerTracerName = "github.com/kennguy3n/zk-drive/internal/tracing"

// ConsumerJobHandler is the per-message handler signature used by
// [WrapConsumer]. It receives the per-message context (with the
// publisher's trace-context propagated) plus the original nats.Msg.
// Return the JobResult string so [WrapConsumer] can mark the span
// status without coupling to the metrics package.
type ConsumerJobHandler func(ctx context.Context, msg *nats.Msg) string

// WrapConsumer wraps a per-message handler with W3C trace-context
// extraction + a consumer-kind span. The returned handler is the
// outer wrapper to install at the js.Subscribe call site so the
// trace publisher-side → consumer-side parent-child relationship
// flows automatically.
//
// Wiring layering with the metrics wrapper:
//
//	subscribe(subject, metrics.InstrumentJob(workerCtx, subject,
//	    tracing.WrapConsumer(subject, tracedHandler)))
//
// metrics.InstrumentJob expects the inner handler's signature to be
// `func(ctx, msg) metrics.JobResult` — the workerCtx it threads
// in becomes the parent of the trace context after extraction.
//
// When tracing.Init installed the no-op tracer (endpoint unset),
// Start returns a no-op span and Extract is a cheap no-op, so the
// wrapper still works correctly and adds negligible overhead.
func WrapConsumer(subject string, h ConsumerJobHandler) func(ctx context.Context, msg *nats.Msg) string {
	return func(ctx context.Context, msg *nats.Msg) string {
		// Extract the publisher-injected trace-context from
		// msg.Header. When the publisher was running an older
		// build without propagation, msg.Header is nil — the
		// propagator handles a nil-but-typed carrier the same
		// way as no headers (no parent context discovered, the
		// consumer span starts a new root).
		if msg != nil && msg.Header != nil {
			ctx = otel.GetTextMapPropagator().Extract(ctx, NATSHeaderCarrier(msg.Header))
		}
		spanCtx, span := otel.Tracer(consumerTracerName).Start(ctx,
			"jetstream.consume "+subject,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				semconv.MessagingSystemKey.String("nats"),
				semconv.MessagingDestinationName(subject),
				semconv.MessagingOperationTypeDeliver,
			),
		)
		defer span.End()

		result := h(spanCtx, msg)
		span.SetAttributes(attribute.String("messaging.job_result", result))
		switch result {
		case "ok", "skip":
			// Leave status unset → default Unset which OTel
			// renders as "Ok" — matches the convention for
			// "the handler did exactly what it intended,
			// whether that included business-level skip or
			// not".
		case "error":
			span.SetStatus(codes.Error, "handler returned error")
		case "dropped":
			span.SetStatus(codes.Error, "poison payload terminated")
		}
		return result
	}
}
