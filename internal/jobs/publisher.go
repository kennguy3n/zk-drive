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
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/tracing"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// StreamName is the JetStream WorkQueue stream that backs every
// drive.* job subject. The worker (cmd/worker) is the single creator
// of this stream (ensureStream); other components — the publisher
// (which inspects consumer depth for heavy-preview backpressure), the
// server's admin health-dashboard stream-depth probe, the compact
// supervisor's readiness barrier, and operator tooling — reference
// this constant so the name has one source of truth and the worker
// and publisher cannot drift on it.
const StreamName = "DRIVE_JOBS"

// ErrPreviewDeferred is returned by PublishPreviewWeighted when the
// heavy preview queue is already at or above its backpressure
// threshold. It is NOT a failure: the caller should surface a
// "preview generating…" placeholder to the client and move on. The
// preview will be (re)generated later — on the next access via the
// on-demand path, or by the preview backfill reconciler — rather than
// piling onto an already-saturated heavy worker fleet.
var ErrPreviewDeferred = errors.New("jobs: heavy preview queue saturated, job deferred")

// publisherTracerName is the instrumentation-scope name. Resolved
// lazily via otel.Tracer on each Start call rather than cached at
// init — a cached tracer would bind to the no-op global provider
// that exists before tracing.Init runs and would never see the
// installed exporter.
const publisherTracerName = "github.com/kennguy3n/zk-drive/internal/jobs"

// Subject constants. Keep these in sync with cmd/worker/main.go — the
// worker uses the same strings when declaring JetStream consumers.
const (
	SubjectPreview   = "drive.preview.generate"
	SubjectScan      = "drive.scan.virus"
	SubjectIndex     = "drive.search.index"
	SubjectArchive   = "drive.archive.cold"
	SubjectRetention = "drive.retention.evaluate"
	SubjectClassify  = "drive.classify.file"

	// Preview priority subjects. Preview generation is split across
	// two subjects so the worker can give paying tiers a larger share
	// of its goroutine budget and a single tenant bulk-uploading
	// cannot starve interactive previews for everyone else:
	//
	//   SubjectPreviewPriority — Business / Secure-Business tiers.
	//   SubjectPreviewStandard — Free / Starter tiers.
	//
	// Both are children of the legacy drive.preview.generate subject
	// name so the DRIVE_JOBS stream's subject list and existing
	// operator dashboards stay grep-able under one "drive.preview.*"
	// prefix. SubjectPreview is retained for backward compatibility:
	// the un-routed PublishPreview path still uses it, and the worker
	// keeps a consumer on it so in-flight jobs published before a
	// rollout are not stranded.
	SubjectPreviewPriority = "drive.preview.generate.priority"
	SubjectPreviewStandard = "drive.preview.generate.standard"

	// Preview weight subjects. Orthogonal to the priority/standard
	// (billing) split above: these route by RENDERER WEIGHT so the two
	// worker pod images can each consume only the work they are built
	// for. The slim server image ships no subprocess binaries and
	// subscribes only to the lightweight subject (pure-Go renderers:
	// images, text, markdown, CSV, archives, email). The heavy worker
	// image ships LibreOffice / FFmpeg / ImageMagick / poppler /
	// librsvg and is the only pod that subscribes to the heavy subject.
	//
	// The dispatcher chooses the subject from preview.IsHeavyMime at
	// publish time, so a DOCX upload never lands on a slim pod that
	// cannot render it (it would otherwise Ack-skip and the file would
	// never get a thumbnail), and an image upload never burns a scarce
	// heavy-pod slot. Both stay under the drive.preview.* prefix so the
	// stream subject filter and dashboards keep one grep-able namespace.
	SubjectPreviewLightweight = "drive.preview.generate.lightweight"
	SubjectPreviewHeavy       = "drive.preview.generate.heavy"
)

// Durable consumer names for the weight-tiered preview subjects. The
// publisher needs the heavy durable to probe queue depth for
// backpressure; the worker uses both when binding its consumers. Shared
// here so the two sides cannot drift.
const (
	DurablePreviewLightweight = "drive-preview-lightweight"
	DurablePreviewHeavy       = "drive-preview-heavy"
)

// PreviewSubjectForTier maps a billing tier name to the NATS subject
// its preview jobs should be published on. Business and Secure-
// Business land on the priority subject; every other tier (including
// the empty / unknown string) falls back to standard so an
// unrecognised tier degrades safely rather than silently gaining
// priority.
func PreviewSubjectForTier(tier string) string {
	switch tier {
	case billing.TierBusiness, billing.TierSecureBusiness:
		return SubjectPreviewPriority
	default:
		return SubjectPreviewStandard
	}
}

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

	// heavyBackpressure is the maximum number of in-flight + pending
	// jobs the heavy preview consumer may have queued before
	// PublishPreviewWeighted starts deferring new heavy jobs. 0
	// disables backpressure (every heavy job is enqueued
	// unconditionally), which is the default.
	heavyBackpressure int
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

// WithHeavyBackpressure sets the heavy-preview-queue depth threshold at
// which PublishPreviewWeighted starts returning ErrPreviewDeferred
// instead of enqueuing. threshold <= 0 disables backpressure. Returns
// the receiver for chaining and is safe to call on a nil *Publisher
// (no-op). Intended to be called once at server startup, before the
// publisher is shared with request handlers.
func (p *Publisher) WithHeavyBackpressure(threshold int) *Publisher {
	if p == nil {
		return nil
	}
	if threshold < 0 {
		threshold = 0
	}
	p.heavyBackpressure = threshold
	return p
}

// PublishPreview enqueues a preview-generation job for the (file,
// version) pair. Safe to call on a nil receiver.
func (p *Publisher) PublishPreview(ctx context.Context, fileID, versionID uuid.UUID) error {
	return p.publish(ctx, SubjectPreview, FileJob{FileID: fileID, VersionID: versionID})
}

// PublishPreviewTier enqueues a preview-generation job on the
// tier-appropriate priority subject (see PreviewSubjectForTier).
// Callers that know the workspace's billing tier at dispatch time use
// this instead of PublishPreview so Business / Secure-Business
// previews are routed to the priority worker pool. Safe to call on a
// nil receiver.
func (p *Publisher) PublishPreviewTier(ctx context.Context, fileID, versionID uuid.UUID, tier string) error {
	return p.publish(ctx, PreviewSubjectForTier(tier), FileJob{FileID: fileID, VersionID: versionID})
}

// PublishPreviewWeighted routes a preview job to the lightweight or
// heavy subject according to the renderer weight of the file's MIME
// type (heavy == true means a subprocess renderer). This is the
// dispatch path used by ConfirmUpload so a job only ever reaches a pod
// that can actually render it.
//
// When heavy is true and a backpressure threshold is configured (see
// WithHeavyBackpressure), the heavy consumer's queue depth is probed
// first. If it is already at or above the threshold the job is NOT
// enqueued and ErrPreviewDeferred is returned so the caller can show a
// "preview generating…" placeholder rather than growing an unbounded
// backlog on the heavy fleet. Lightweight jobs are never deferred —
// pure-Go renders are cheap and the slim pool scales horizontally.
//
// Safe to call on a nil receiver (no-op returning nil).
func (p *Publisher) PublishPreviewWeighted(ctx context.Context, fileID, versionID uuid.UUID, heavy bool) error {
	if p == nil || p.js == nil {
		return nil
	}
	if !heavy {
		return p.publish(ctx, SubjectPreviewLightweight, FileJob{FileID: fileID, VersionID: versionID})
	}
	if p.heavyOverThreshold(ctx) {
		return ErrPreviewDeferred
	}
	return p.publish(ctx, SubjectPreviewHeavy, FileJob{FileID: fileID, VersionID: versionID})
}

// heavyOverThreshold reports whether the heavy preview consumer's
// backlog (pending + unacked) is at or above the configured
// backpressure threshold. It FAILS OPEN: a zero/disabled threshold, a
// missing consumer (worker not yet started), or any JetStream probe
// error all return false so a backpressure-probe hiccup never blocks a
// legitimate preview. Backpressure is a load-shedding optimisation, not
// a correctness gate.
func (p *Publisher) heavyOverThreshold(ctx context.Context) bool {
	if p.heavyBackpressure <= 0 {
		return false
	}
	info, err := p.js.ConsumerInfo(StreamName, DurablePreviewHeavy, nats.Context(ctx))
	if err != nil || info == nil {
		return false
	}
	// NumPending is messages not yet delivered; NumAckPending is
	// messages delivered but not yet Ack'd (currently rendering or
	// awaiting redelivery). Their sum is the true outstanding heavy
	// workload the fleet still owes.
	depth := int(info.NumPending) + info.NumAckPending
	return depth >= p.heavyBackpressure
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
	otel.GetTextMapPropagator().Inject(ctx, tracing.NATSHeaderCarrier(msg.Header))
	if _, err := p.js.PublishMsgAsync(msg, nats.Context(ctx)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish failed")
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}
