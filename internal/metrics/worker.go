package metrics

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"
)

// JobResult classifies the outcome of a worker job for metrics
// purposes. The four values mirror the existing
// cmd/worker/main.go control flow:
//
//   - JobResultOK     — the handler did the work and called Ack().
//                       This is the happy path counted in SLO
//                       calculations.
//   - JobResultSkip   — the handler intentionally did NOT process
//                       the message and called Ack(). Two
//                       in-product reasons: the file lives in a
//                       strict-ZK folder (preview / scan / index /
//                       classify all skip-and-ack those), or the
//                       worker booted without the dependent
//                       service (no storage client → ack and move
//                       on rather than queue garbage). Counting
//                       these separately from "ok" prevents
//                       deployment misconfigurations from being
//                       hidden behind a healthy-looking success
//                       rate.
//   - JobResultError  — the handler hit a transient failure and
//                       called Nak() so NATS will redeliver after
//                       AckWait. High-cardinality on this label
//                       value is the "page someone" signal.
//   - JobResultDropped — the handler hit a poison-payload error
//                       and called Term() so NATS will NOT
//                       redeliver. This should be rare; a non-
//                       zero rate is the signal that some
//                       publisher is emitting malformed envelopes.
type JobResult string

// JobResult* constants used as the second label on
// zkdrive_worker_jobs_total. Exported so cmd/worker handlers
// can return them by name rather than by string literal.
const (
	JobResultOK      JobResult = "ok"
	JobResultSkip    JobResult = "skip"
	JobResultError   JobResult = "error"
	JobResultDropped JobResult = "dropped"
)

// JobHandler is a metrics-friendly variant of nats.MsgHandler.
// The handler is still responsible for calling msg.Ack / Nak /
// Term itself — the wrapper does NOT take over ack semantics
// because mid-handler ack timing (e.g. ack-then-write-DB vs.
// write-DB-then-ack) is a per-job correctness decision that
// belongs to the handler. The handler returns the JobResult so
// the wrapper can emit metrics with the right label.
//
// The ctx parameter is the per-message context: it carries the
// W3C trace-context extracted from msg.Header (so DB / Redis
// / downstream calls reuse the publisher-side trace id) and
// retains the worker's shutdown cancel signal. Handlers should
// derive a per-job timeout from this context rather than from a
// closure-captured root ctx so trace propagation works.
type JobHandler func(ctx context.Context, msg *nats.Msg) JobResult

// InstrumentJob wraps a JobHandler with the worker_jobs_total
// counter (labelled by subject + result) and the
// worker_job_duration_seconds histogram (labelled by subject).
// The returned nats.MsgHandler is the value to pass to
// js.Subscribe.
//
// workerCtx is the worker's root context (carrying the shutdown
// cancel signal). InstrumentJob passes it through to the
// handler as-is unless a per-message context wrapper (e.g.
// tracing.WrapConsumer) was installed first.
//
// Subject is taken as a constant (not pulled off msg.Subject)
// because msg.Subject can include wildcard token expansions on
// some NATS deployments — the constant from internal/jobs is
// the bounded label space we want.
func (m *Metrics) InstrumentJob(workerCtx context.Context, subject string, h JobHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		start := time.Now()
		result := h(workerCtx, msg)
		elapsed := time.Since(start).Seconds()

		m.workerJobsTotal.WithLabelValues(subject, string(result)).Inc()
		m.workerJobDuration.WithLabelValues(subject).Observe(elapsed)
	}
}
