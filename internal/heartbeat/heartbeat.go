// Package heartbeat gives the API server a pull-based liveness signal
// for the background worker fleet.
//
// The worker process and the API server are decoupled over JetStream
// and never talk directly, so the server historically had no way to
// answer the operator question "are my workers actually running?".
// This package closes that gap with a tiny worker_heartbeats table
// (migration 039): each worker process periodically upserts one row
// per logical worker type it runs (the Recorder), and the server's
// admin health dashboard reads the freshest row per type (the Store
// reader) to render a green / yellow / red signal.
//
// The design is deliberately database-backed rather than a NATS
// request/reply: the dashboard must be able to report "no workers
// have checked in" even when the message bus itself is the thing
// that is down, which a bus-based liveness probe cannot do.
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultInterval is the cadence at which the Recorder refreshes its
// heartbeat rows. 15s is frequent enough that the dashboard's
// staleness thresholds (see StaleAfter / DeadAfter) react within a
// minute of a worker wedging, while being far too infrequent to
// matter as database write load (a handful of upserts per worker
// process per 15s).
const DefaultInterval = 15 * time.Second

// StaleAfter is how long a worker type may go without refreshing its
// heartbeat before the dashboard downgrades it to "stale" (yellow).
// Set to 3× DefaultInterval so a single missed beat (e.g. a GC pause
// or a slow upsert) does not flap the signal; three consecutive
// misses is a real problem worth surfacing.
const StaleAfter = 3 * DefaultInterval

// DeadAfter is how long a worker type may go silent before the
// dashboard treats it as down (red). 8× the interval (two minutes at
// the default cadence) distinguishes "briefly stale" from "the
// process is gone".
const DeadAfter = 8 * DefaultInterval

// PruneRetention is how long a heartbeat row may persist after its
// last refresh before Prune deletes it. A restarted worker gets a new
// pid-based instance_id (see InstanceID), so its old row is never
// overwritten — without pruning the table grows by one row per process
// restart, unbounded across rolling deploys. The threshold sits far
// beyond DeadAfter (two minutes) so a recently crashed worker still
// shows red on the dashboard for diagnosis; only long-gone instances
// are reaped.
const PruneRetention = 24 * time.Hour

// PruneInterval is how often the Recorder reaps rows older than
// PruneRetention. Hourly is ample: the table only accretes a row per
// restart, so cleanup cadence is not latency-sensitive. Every worker
// process prunes independently; the DELETE is idempotent so concurrent
// pruners are harmless.
const PruneInterval = time.Hour

// Status is the worker-type health a producer reports. Kept as a
// small closed set of strings that map directly onto the dashboard's
// traffic-light colours.
type Status string

const (
	// StatusOK — the worker type is running normally.
	StatusOK Status = "ok"
	// StatusDegraded — running but with a dependency disabled (e.g.
	// virus scanning auto-disabled because ClamAV is unreachable).
	StatusDegraded Status = "degraded"
)

// Beat is a single heartbeat a worker emits for one logical worker
// type. Detail is an optional bag of type-specific context (e.g.
// {"virus_scanning": false}); it is serialised to the JSONB detail
// column verbatim.
type Beat struct {
	WorkerType string
	Status     Status
	Detail     map[string]any
}

// WorkerHealth is the aggregated view of one worker type across every
// reporting instance, as consumed by the health dashboard.
type WorkerHealth struct {
	WorkerType string         `json:"worker_type"`
	Instances  int            `json:"instances"`
	LastSeenAt time.Time      `json:"last_seen_at"`
	Status     Status         `json:"status"`
	Detail     map[string]any `json:"detail,omitempty"`
}

// Store reads and writes worker heartbeats. Safe for concurrent use:
// it holds only a *pgxpool.Pool, which is itself concurrency-safe.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pgx pool. A nil pool yields a Store whose methods
// return an error / empty result rather than panicking, so callers
// in dependency-light deployments (no Postgres wired in a test) stay
// safe.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Upsert records (or refreshes) the heartbeat for a single
// (worker_type, instance) pair. last_seen_at is set to now() server
// side so all instances share the database clock rather than their
// own possibly-skewed wall clocks.
func (s *Store) Upsert(ctx context.Context, instanceID string, b Beat) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("heartbeat: store not initialised")
	}
	status := b.Status
	if status == "" {
		status = StatusOK
	}
	detail := b.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("heartbeat: marshal detail: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO worker_heartbeats (worker_type, instance_id, last_seen_at, status, detail)
		VALUES ($1, $2, now(), $3, $4::jsonb)
		ON CONFLICT (worker_type, instance_id)
		DO UPDATE SET last_seen_at = now(), status = EXCLUDED.status, detail = EXCLUDED.detail
	`, b.WorkerType, instanceID, string(status), string(detailJSON))
	if err != nil {
		return fmt.Errorf("heartbeat: upsert: %w", err)
	}
	return nil
}

// List returns the aggregated health of every worker type that has
// ever reported, sorted by worker type for stable rendering. The
// returned Status is the WORST across that type's instances (degraded
// beats ok), and LastSeenAt is the FRESHEST across instances (the
// type is only as stale as its liveliest replica). Staleness is NOT
// applied here — the dashboard layer owns the StaleAfter / DeadAfter
// policy so the reader stays a pure data accessor.
func (s *Store) List(ctx context.Context) ([]WorkerHealth, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("heartbeat: store not initialised")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT worker_type, instance_id, last_seen_at, status, detail
		FROM worker_heartbeats
	`)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: query: %w", err)
	}
	defer rows.Close()

	type agg struct {
		instances  int
		lastSeen   time.Time
		degraded   bool
		detail     map[string]any
		detailSeen time.Time
	}
	byType := map[string]*agg{}
	for rows.Next() {
		var (
			workerType, instanceID, status string
			lastSeen                       time.Time
			detailRaw                      []byte
		)
		if err := rows.Scan(&workerType, &instanceID, &lastSeen, &status, &detailRaw); err != nil {
			return nil, fmt.Errorf("heartbeat: scan: %w", err)
		}
		a := byType[workerType]
		if a == nil {
			a = &agg{}
			byType[workerType] = a
		}
		a.instances++
		if lastSeen.After(a.lastSeen) {
			a.lastSeen = lastSeen
		}
		if Status(status) == StatusDegraded {
			a.degraded = true
		}
		// Keep the detail from the freshest instance so the operator
		// sees the most current context for the type.
		if lastSeen.After(a.detailSeen) && len(detailRaw) > 0 {
			var d map[string]any
			if json.Unmarshal(detailRaw, &d) == nil {
				a.detail = d
				a.detailSeen = lastSeen
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("heartbeat: rows: %w", err)
	}

	out := make([]WorkerHealth, 0, len(byType))
	for workerType, a := range byType {
		status := StatusOK
		if a.degraded {
			status = StatusDegraded
		}
		out = append(out, WorkerHealth{
			WorkerType: workerType,
			Instances:  a.instances,
			LastSeenAt: a.lastSeen,
			Status:     status,
			Detail:     a.detail,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkerType < out[j].WorkerType })
	return out, nil
}

// Prune deletes heartbeat rows whose last_seen_at is older than
// PruneRetention, bounding the table against the one-row-per-restart
// growth that the pid-based instance_id would otherwise cause. Returns
// the number of rows reaped. Idempotent and safe to call concurrently
// from every worker process; the threshold is computed server-side via
// make_interval so all instances share the database clock.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("heartbeat: store not initialised")
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM worker_heartbeats
		WHERE last_seen_at < now() - make_interval(secs => $1)
	`, PruneRetention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("heartbeat: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

// InstanceID builds a stable-per-process instance identifier
// (hostname + pid). Two replicas on different hosts, or two processes
// on one host, never collide; a restarted process gets a new pid and
// its old row simply ages out via the staleness policy (and is
// overwritten if the host reuses the pid).
func InstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return host + "/" + strconv.Itoa(os.Getpid())
}
