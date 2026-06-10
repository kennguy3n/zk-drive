package jobs

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestNewPublisherNilJSReturnsNil pins the documented contract: a
// nil JetStream context yields a nil *Publisher, NOT a populated
// pointer that happens to swallow errors. The API handler code calls
// methods on the pointer unconditionally — if NewPublisher(nil)
// returned &Publisher{js: nil} instead of nil, the internal nil
// check on `p.js` would still work, but callers that grep for
// "publisher == nil" as a feature-flag would silently see "publisher
// enabled" and emit metrics / logs that don't match reality.
func TestNewPublisherNilJSReturnsNil(t *testing.T) {
	if got := NewPublisher(nil); got != nil {
		t.Fatalf("NewPublisher(nil) = %v, want nil", got)
	}
}

// TestPublisherNilReceiverMethods exercises every Publish* call on
// a nil *Publisher. Each must return nil error so API handlers that
// hold a nil publisher can call these unconditionally — the entire
// design relies on "no NATS == no jobs == no panics".
func TestPublisherNilReceiverMethods(t *testing.T) {
	var p *Publisher
	ctx := context.Background()
	fileID := uuid.New()
	versionID := uuid.New()

	if err := p.PublishPreview(ctx, fileID, versionID); err != nil {
		t.Fatalf("PublishPreview on nil receiver: %v", err)
	}
	if err := p.PublishPreviewWeighted(ctx, fileID, versionID, true); err != nil {
		t.Fatalf("PublishPreviewWeighted on nil receiver: %v", err)
	}
	if err := p.PublishScan(ctx, fileID, versionID); err != nil {
		t.Fatalf("PublishScan on nil receiver: %v", err)
	}
	if err := p.PublishIndex(ctx, fileID, versionID); err != nil {
		t.Fatalf("PublishIndex on nil receiver: %v", err)
	}
	if err := p.PublishArchive(ctx, fileID, versionID); err != nil {
		t.Fatalf("PublishArchive on nil receiver: %v", err)
	}
	if err := p.PublishClassify(ctx, fileID, versionID); err != nil {
		t.Fatalf("PublishClassify on nil receiver: %v", err)
	}
}

// TestSubjectConstants pins every public subject string. These are
// part of the wire contract with cmd/worker — any drift here breaks
// JetStream consumer subscription. The constants live in two places
// (this package and cmd/worker/main.go) so a regression test here
// catches accidental edits even before the worker rebuilds.
func TestSubjectConstants(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{SubjectPreview, "drive.preview.generate"},
		{SubjectScan, "drive.scan.virus"},
		{SubjectIndex, "drive.search.index"},
		{SubjectArchive, "drive.archive.cold"},
		{SubjectRetention, "drive.retention.evaluate"},
		{SubjectClassify, "drive.classify.file"},
	} {
		if tc.got != tc.want {
			t.Fatalf("subject drift: got %q want %q", tc.got, tc.want)
		}
	}
}
