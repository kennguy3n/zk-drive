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
)

// Subject constants. Keep these in sync with cmd/worker/main.go — the
// worker uses the same strings when declaring JetStream consumers.
const (
	SubjectPreview = "drive.preview.generate"
	SubjectScan    = "drive.scan.virus"
	SubjectIndex   = "drive.search.index"
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

func (p *Publisher) publish(ctx context.Context, subject string, payload FileJob) error {
	if p == nil || p.js == nil {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", subject, err)
	}
	if _, err := p.js.PublishAsync(subject, body, nats.Context(ctx)); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}
