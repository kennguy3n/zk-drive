// Package jobs provides the NATS JetStream publisher used by the API
// server to hand off asynchronous work (preview generation, virus
// scanning, search indexing) to the worker binary. The actual
// consumer side lives in cmd/worker.
//
// Design notes:
//   - The publisher is deliberately nil-safe: API handlers hold a
//     *Publisher and call its methods unconditionally; when the server
//     started without NATS configured the pointer is nil and every
//     method is a no-op, just like the existing logActivity wrapper.
//   - Subjects are stable strings so the worker can declare its
//     consumers once and the publisher does not need to know about
//     stream topology.
//   - Payloads are small JSON envelopes keyed by file_id and version_id.
//     Workers re-read metadata from Postgres before doing work, so
//     payload bloat is not a correctness concern.
package jobs

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
)

// natsMsgCarrier adapts nats.Header to the OpenTelemetry
// propagation.TextMapCarrier interface so the global propagator can
// inject the W3C trace-context (traceparent / tracestate) and
// baggage onto an outgoing NATS message. The consumer then extracts
// the same headers in cmd/worker, recreating a parent-child span
// relationship across the network boundary. NATS supports per-message
// headers since 2.2 (and JetStream since 2.3.4), which our minimum
// required server version already exceeds.
type natsMsgCarrier nats.Header

func (c natsMsgCarrier) Get(key string) string  { return nats.Header(c).Get(key) }
func (c natsMsgCarrier) Set(key, value string)  { nats.Header(c).Set(key, value) }
func (c natsMsgCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// publisherTracerName is the instrumentation-scope name. Resolved
// lazily via otel.Tracer on each Start call rather than cached at
// init — a cached tracer would bind to the no-op global provider
// that exists before tracing.Init runs and would never see the
// installed exporter.
const publisherTracerName = "github.com/kennguy3n/zk-drive/internal/jobs"

// Subject constants. Keep these in sync with cmd/worker/main.go — the
// worker uses the same strings when declaring JetStream consumers.
const (
	SubjectPreview  = "drive.preview.generate"
	SubjectScan     = "drive.scan.virus"
	SubjectIndex    = "drive.search.index"
	SubjectArchive  = "drive.archive.cold"
	SubjectRetention = "drive.retention.evaluate"
	SubjectClassify  = "drive.classify.file"
)

// FileJob is the common payload shape for every drive.* subject. We
// keep it tiny and keyed by ids so the worker re-hydrates the latest
// file / version state from Postgres rather than trusting stale
// payload fields.
type FileJob struct {
	FileID    uuid.UUID `json:"file_id"`
	VersionID uuid.UUID `json:"version_id"`
}

// Publisher wraps a JetStream context and exposes typed publishers for
// each subject. A nil *Publisher is a valid no-op receiver so API
// handlers can call methods unconditionally without null-checking.
type Publisher struct {
	js nats.JetStreamContext
}

// NewPublisher constructs a Publisher bound to the supplied JetStream
// context. Pass nil to disable publishing — every method becomes a
// no-op returning nil.
func NewPublisher(js nats.JetStreamContext) *Publisher {
	if js == nil {
		return nil
	}
	return &Publisher{js: js}
}

// PublishPreview enqueues a preview-generation job for the (file,
// version) pair. Safe to call on a nil receiver.
func (p *Publisher) PublishPreview(ctx context.Context, fileID, versionID uuid.UUID) error {
	return p.publish(ctx, SubjectPreview, FileJob{FileID: fileID, VersionID: versionID})
}

// PublishScan enqueues a virus-scan job. Safe to call on a nil receiver.
func (p *Publisher) PublishScan(ctx context.Context, fileID, versionID uuid.UUID) error {
	return p.publish(ctx, SubjectScan, FileJob{FileID: fileID, VersionID: versionID})
}

// PublishIndex enqueues a search-index job. Safe to call on a nil receiver.
func (p *Publisher) PublishIndex(ctx context.Context, fileID, versionID uuid.UUID) error {
	return p.publish(ctx, SubjectIndex, FileJob{FileID: fileID, VersionID: versionID})
}

// PublishArchive enqueues a cold-archive job for a version. Safe to
// call on a nil receiver.
func (p *Publisher) PublishArchive(ctx context.Context, fileID, versionID uuid.UUID) error {
	return p.publish(ctx, SubjectArchive, FileJob{FileID: fileID, VersionID: versionID})
}

// PublishClassify enqueues a rule-based classification job for the
// (file, version) pair. Safe to call on a nil receiver.
func (p *Publisher) PublishClassify(ctx context.Context, fileID, versionID uuid.UUID) error {
	return p.publish(ctx, SubjectClassify, FileJob{FileID: fileID, VersionID: versionID})
}

func (p *Publisher) publish(ctx context.Context, subject string, payload FileJob) error {
	if p == nil || p.js == nil {
		return nil
	}
	// Wrap the publish in a producer-kind span so the resulting
	// trace shows "API request → publish job" as a parent of
	// "worker → consume job" once the consumer extracts the
	// propagated context. Attributes follow the messaging.*
	// semantic conventions so any OTel-aware backend can group
	// publisher and consumer spans by destination subject without
	// custom mapping.
	ctx, span := otel.Tracer(publisherTracerName).Start(ctx,
		"jetstream.publish "+subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemKey.String("nats"),
			semconv.MessagingDestinationName(subject),
			semconv.MessagingOperationTypePublish,
			attribute.String("messaging.nats.file_id", payload.FileID.String()),
			attribute.String("messaging.nats.version_id", payload.VersionID.String()),
		),
	)
	defer span.End()

	body, err := json.Marshal(payload)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal payload")
		return fmt.Errorf("marshal %s payload: %w", subject, err)
	}
	// Inject the W3C trace-context onto the outgoing message via
	// nats.Msg headers so the consumer recreates the parent-child
	// link. Using PublishMsgAsync rather than PublishAsync because
	// the latter has no header-passing equivalent.
	msg := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{}}
	otel.GetTextMapPropagator().Inject(ctx, natsMsgCarrier(msg.Header))
	if _, err := p.js.PublishMsgAsync(msg, nats.Context(ctx)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish failed")
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}
