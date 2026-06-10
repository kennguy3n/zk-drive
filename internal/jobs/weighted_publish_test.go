package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// fakeJS is a minimal nats.JetStreamContext used to exercise the
// weight-routing and backpressure logic without a live broker. Only the
// two methods PublishPreviewWeighted touches are implemented; every
// other interface method is left nil via the embedded interface and
// would panic if called, which is exactly what we want — a test that
// accidentally exercises an unmocked path fails loudly rather than
// silently passing.
type fakeJS struct {
	nats.JetStreamContext

	info     *nats.ConsumerInfo
	infoErr  error
	infoReqs int

	published []*nats.Msg
}

func (f *fakeJS) ConsumerInfo(stream, durable string, _ ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	f.infoReqs++
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	return f.info, nil
}

func (f *fakeJS) PublishMsgAsync(m *nats.Msg, _ ...nats.PubOpt) (nats.PubAckFuture, error) {
	f.published = append(f.published, m)
	return nil, nil
}

func consumerInfo(numPending uint64, numAckPending int) *nats.ConsumerInfo {
	return &nats.ConsumerInfo{NumPending: numPending, NumAckPending: numAckPending}
}

// TestPublishPreviewWeightedRouting pins the subject each weight maps
// to: lightweight MIMEs land on the lightweight subject, heavy on the
// heavy subject. Routing is the whole point of the weight split, so a
// regression here would silently send DOCX jobs to slim pods.
func TestPublishPreviewWeightedRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		heavy       bool
		wantSubject string
	}{
		{"light", false, SubjectPreviewLightweight},
		{"heavy", true, SubjectPreviewHeavy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			js := &fakeJS{}
			p := NewPublisher(js)
			if err := p.PublishPreviewWeighted(context.Background(), uuid.New(), uuid.New(), tc.heavy); err != nil {
				t.Fatalf("PublishPreviewWeighted: %v", err)
			}
			if len(js.published) != 1 {
				t.Fatalf("published %d messages, want 1", len(js.published))
			}
			if got := js.published[0].Subject; got != tc.wantSubject {
				t.Fatalf("subject = %q, want %q", got, tc.wantSubject)
			}
		})
	}
}

// TestPublishPreviewWeightedBackpressureDisabled confirms a zero
// threshold never probes the consumer and always enqueues — the
// default, opt-in posture.
func TestPublishPreviewWeightedBackpressureDisabled(t *testing.T) {
	js := &fakeJS{info: consumerInfo(1_000_000, 0)}
	p := NewPublisher(js) // no WithHeavyBackpressure → threshold 0
	if err := p.PublishPreviewWeighted(context.Background(), uuid.New(), uuid.New(), true); err != nil {
		t.Fatalf("PublishPreviewWeighted: %v", err)
	}
	if js.infoReqs != 0 {
		t.Fatalf("ConsumerInfo called %d times with backpressure disabled, want 0", js.infoReqs)
	}
	if len(js.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(js.published))
	}
}

// TestPublishPreviewWeightedBackpressureDefers asserts that once the
// heavy queue depth (pending + unacked) reaches the threshold, the job
// is deferred (ErrPreviewDeferred) and NOT published.
func TestPublishPreviewWeightedBackpressureDefers(t *testing.T) {
	js := &fakeJS{info: consumerInfo(8, 2)} // depth 10
	p := NewPublisher(js).WithHeavyBackpressure(10)
	err := p.PublishPreviewWeighted(context.Background(), uuid.New(), uuid.New(), true)
	if !errors.Is(err, ErrPreviewDeferred) {
		t.Fatalf("err = %v, want ErrPreviewDeferred", err)
	}
	if len(js.published) != 0 {
		t.Fatalf("published %d messages while deferred, want 0", len(js.published))
	}
}

// TestPublishPreviewWeightedBackpressureAdmits confirms a depth just
// under the threshold still enqueues.
func TestPublishPreviewWeightedBackpressureAdmits(t *testing.T) {
	js := &fakeJS{info: consumerInfo(8, 1)} // depth 9 < 10
	p := NewPublisher(js).WithHeavyBackpressure(10)
	if err := p.PublishPreviewWeighted(context.Background(), uuid.New(), uuid.New(), true); err != nil {
		t.Fatalf("PublishPreviewWeighted: %v", err)
	}
	if len(js.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(js.published))
	}
}

// TestPublishPreviewWeightedBackpressureFailsOpen confirms that a
// ConsumerInfo probe error does NOT block the publish: backpressure is
// a load-shedding optimisation, never a correctness gate.
func TestPublishPreviewWeightedBackpressureFailsOpen(t *testing.T) {
	js := &fakeJS{infoErr: errors.New("consumer not found")}
	p := NewPublisher(js).WithHeavyBackpressure(10)
	if err := p.PublishPreviewWeighted(context.Background(), uuid.New(), uuid.New(), true); err != nil {
		t.Fatalf("PublishPreviewWeighted should fail open, got: %v", err)
	}
	if len(js.published) != 1 {
		t.Fatalf("published %d messages, want 1 (fail-open)", len(js.published))
	}
}

// TestPublishPreviewWeightedLightweightNeverDeferred confirms the
// lightweight path skips the backpressure probe entirely — pure-Go
// renders are cheap and the slim pool scales horizontally.
func TestPublishPreviewWeightedLightweightNeverDeferred(t *testing.T) {
	js := &fakeJS{info: consumerInfo(1_000_000, 0)}
	p := NewPublisher(js).WithHeavyBackpressure(1)
	if err := p.PublishPreviewWeighted(context.Background(), uuid.New(), uuid.New(), false); err != nil {
		t.Fatalf("PublishPreviewWeighted(light): %v", err)
	}
	if js.infoReqs != 0 {
		t.Fatalf("ConsumerInfo probed %d times for a lightweight job, want 0", js.infoReqs)
	}
	if len(js.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(js.published))
	}
}

// TestWithHeavyBackpressureNilAndClamp pins the two guard rails:
// calling on a nil receiver is a no-op (no panic), and a negative
// threshold clamps to 0 (disabled).
func TestWithHeavyBackpressureNilAndClamp(t *testing.T) {
	var nilp *Publisher
	if got := nilp.WithHeavyBackpressure(5); got != nil {
		t.Fatalf("nil.WithHeavyBackpressure = %v, want nil", got)
	}

	js := &fakeJS{info: consumerInfo(1_000_000, 0)}
	p := NewPublisher(js).WithHeavyBackpressure(-3)
	if p.heavyBackpressure != 0 {
		t.Fatalf("negative threshold = %d, want clamped to 0", p.heavyBackpressure)
	}
	// With threshold clamped to 0, a huge queue still admits.
	if err := p.PublishPreviewWeighted(context.Background(), uuid.New(), uuid.New(), true); err != nil {
		t.Fatalf("PublishPreviewWeighted: %v", err)
	}
	if len(js.published) != 1 {
		t.Fatalf("published %d, want 1", len(js.published))
	}
}

// TestWeightSubjectConstants pins the new wire-contract subjects and
// durables shared with cmd/worker.
func TestWeightSubjectConstants(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{SubjectPreviewLightweight, "drive.preview.generate.lightweight"},
		{SubjectPreviewHeavy, "drive.preview.generate.heavy"},
		{DurablePreviewLightweight, "drive-preview-lightweight"},
		{DurablePreviewHeavy, "drive-preview-heavy"},
		{StreamName, "DRIVE_JOBS"},
	} {
		if tc.got != tc.want {
			t.Fatalf("constant drift: got %q want %q", tc.got, tc.want)
		}
	}
}
