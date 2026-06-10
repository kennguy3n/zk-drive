package heartbeat

import (
	"context"
	"log/slog"
	"time"
)

// BeatProvider returns the current set of heartbeats a worker process
// wants to publish. It is invoked once per tick so the reported
// status can change over the process's lifetime — e.g. the scan
// worker flips from StatusOK to StatusDegraded the moment its
// self-healing loop disables virus scanning, and the dashboard
// reflects that on the next read without a restart.
type BeatProvider func() []Beat

// Recorder periodically writes a worker process's heartbeats to the
// Store. Construct via NewRecorder and drive with Run; the loop owns
// no goroutine of its own so the caller controls its lifecycle via
// the supplied context (typically the worker's root context).
type Recorder struct {
	store      *Store
	instanceID string
	interval   time.Duration
	provider   BeatProvider
	logger     *slog.Logger
}

// NewRecorder builds a Recorder. A zero or negative interval falls
// back to DefaultInterval. A nil logger falls back to slog.Default so
// the recorder never needs a nil-check at the call site.
func NewRecorder(store *Store, instanceID string, interval time.Duration, provider BeatProvider, logger *slog.Logger) *Recorder {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{
		store:      store,
		instanceID: instanceID,
		interval:   interval,
		provider:   provider,
		logger:     logger,
	}
}

// Run writes an immediate heartbeat, then refreshes on every tick
// until ctx is cancelled. It returns when ctx is done. A failed
// upsert is logged at warn and otherwise ignored: a transient
// database hiccup must never crash the worker, and the dashboard's
// staleness policy already degrades the signal if writes stop
// landing. Run is intended to be launched in its own goroutine
// (`go rec.Run(ctx)`).
func (r *Recorder) Run(ctx context.Context) {
	if r == nil || r.store == nil || r.provider == nil {
		return
	}
	r.writeOnce(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.writeOnce(ctx)
		}
	}
}

func (r *Recorder) writeOnce(ctx context.Context) {
	for _, b := range r.provider() {
		if err := r.store.Upsert(ctx, r.instanceID, b); err != nil {
			// ctx cancellation during shutdown is expected; don't log
			// it as a failure.
			if ctx.Err() != nil {
				return
			}
			r.logger.Warn("worker heartbeat upsert failed",
				"worker_type", b.WorkerType, "err", err)
		}
	}
}
