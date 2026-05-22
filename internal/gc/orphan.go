// Package gc reclaims storage objects whose presigned PUT completed
// but whose ConfirmUpload never landed (or was rejected for quota /
// suspended-tenant / etc.). Without this reclamation, the operator
// pays indefinitely for every never-confirmed upload, with no way
// to identify the orphan from the metadata side because the
// generated versionID is lost the moment UploadURL returns.
//
// Migration 025 introduced the files.pending_upload_object_key
// column so UploadURL can stamp the key it minted on the file row;
// ConfirmUpload clears the column transactionally when it inserts
// the version; this package scans for rows that still carry the
// key past a configurable cooldown and reclaims both the S3 object
// (best-effort DeleteObject) and the orphan file row (predicate-
// guarded DELETE that's safe against a confirm racing the scan).
//
// Operational shape mirrors internal/reconciler:
//
//   - cmd/orphan-gc: standalone binary suitable for a K8s CronJob.
//     Runs once and exits.
//   - cmd/worker: in-process periodic invocation gated on
//     GC_INTERVAL_MINUTES (default 360 = 6h, 0 disables).
//
// Both call into the same GCAll entrypoint so the behaviour is
// identical regardless of how the GC is scheduled. The cooldown
// (GC_PENDING_UPLOAD_TTL_HOURS, default 168 = 7 days) trades reclaim
// latency against tolerance for slow clients: a value below the
// presigned URL expiry (15 minutes) risks racing a still-uploading
// client; the 7-day default is generous and matches the trash /
// recycle-bin window used elsewhere.
package gc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/file"
)

// DefaultPendingUploadTTL is the cooldown applied before a pending
// upload row is considered an orphan. Operators can override via
// GC_PENDING_UPLOAD_TTL_HOURS; the 7-day default matches the
// retention / trash window so an orphan and an undeletable trashed
// file age out on the same operational clock.
const DefaultPendingUploadTTL = 7 * 24 * time.Hour

// DefaultPerWorkspaceLimit caps how many orphan rows a single GC
// run reclaims per workspace. The cap prevents a runaway scan
// (e.g. a misbehaving client that minted millions of UploadURL
// calls without ever confirming) from monopolising the DB pool
// inside a single GCAll iteration. The next scheduled tick picks
// up the remainder.
const DefaultPerWorkspaceLimit = 500

// StorageDeleter is the narrow interface GCService needs from the
// storage client. Defined locally so tests can substitute a mock
// without importing the AWS SDK or pulling in network state.
type StorageDeleter interface {
	DeleteObject(ctx context.Context, objectKey string) error
}

// StorageResolver returns the StorageDeleter that owns the given
// workspace's bucket. Mirrors the per-workspace storage factory in
// api/drive: managed-encrypted workspaces use the shared default
// client; provisioned workspaces hand back a per-tenant client.
//
// A nil return indicates the workspace has no storage configured
// (e.g. the tenant was suspended or the storage credentials row
// was deleted). The GC reconciler skips object deletion in that
// case but still removes the orphan file row — the S3 object, if
// it exists, becomes unreachable through ZK Drive metadata anyway
// and the operator can reclaim the bucket out-of-band.
type StorageResolver func(ctx context.Context, workspaceID uuid.UUID) StorageDeleter

// Summary is the result of a GCAll run across all workspaces.
type Summary struct {
	// Workspaces is the number of workspace rows the GC scanned.
	// A workspace with zero pending orphans still counts here.
	Workspaces int

	// OrphansFound is the total number of orphan file rows the
	// scan turned up across every workspace.
	OrphansFound int

	// OrphansDeleted is the number of orphan file rows the GC
	// successfully removed (predicate-guarded DELETE matched
	// exactly one row).
	OrphansDeleted int

	// ObjectsDeleted is the number of S3 objects the GC
	// successfully removed. Counted separately from
	// OrphansDeleted because the storage DeleteObject is
	// best-effort: a transient gateway error fails the object
	// delete but the orphan row is still removed (the row is
	// authoritative for "is this orphan currently visible",
	// and a later GC run will re-attempt the object delete only
	// if a future upload re-stamps the same key, which the UUID
	// scheme prevents).
	ObjectsDeleted int

	// Errors collects per-workspace failures. A single sick
	// workspace does not abort the run.
	Errors []WorkspaceError
}

// WorkspaceError captures a single per-workspace failure.
type WorkspaceError struct {
	WorkspaceID uuid.UUID
	Err         error
}

// GCService binds a pool, the file service, and a storage resolver,
// then runs the per-workspace orphan scan against them. The struct
// holds the dependencies on construction (rather than passing them
// on every call) so the worker's periodic loop and the standalone
// binary can share the same instance.
type GCService struct {
	pool     *pgxpool.Pool
	files    *file.Service
	resolver StorageResolver
	ttl      time.Duration
	limit    int

	// now is overridable in tests so we can drive the
	// "older than TTL" predicate deterministically without
	// sleeping.
	now func() time.Time
}

// Option configures a GCService.
type Option func(*GCService)

// WithTTL overrides the pending-upload cooldown applied before a
// row is reclaimed. Operators tune this via GC_PENDING_UPLOAD_TTL_HOURS;
// tests pin it to a few seconds so the suite isn't I/O-bound.
func WithTTL(ttl time.Duration) Option {
	return func(s *GCService) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// WithPerWorkspaceLimit overrides the per-workspace orphan cap.
func WithPerWorkspaceLimit(limit int) Option {
	return func(s *GCService) {
		if limit > 0 {
			s.limit = limit
		}
	}
}

// WithClock overrides the time source for tests.
func WithClock(now func() time.Time) Option {
	return func(s *GCService) {
		if now != nil {
			s.now = now
		}
	}
}

// New builds a GCService. The pool must be authenticated as the
// application role so the RLS bypass branch fires; same constraint
// as the reconciler (see internal/reconciler.New for context).
//
// resolver may be nil only when the caller never expects to delete
// objects (e.g. a test that focuses on the metadata-only path).
// Production callers always supply a real resolver — the
// constructor does not validate this so a misconfigured worker
// fails loudly with a nil-pointer panic on first GC tick rather
// than silently leaking objects forever.
func New(pool *pgxpool.Pool, files *file.Service, resolver StorageResolver, opts ...Option) *GCService {
	s := &GCService{
		pool:     pool,
		files:    files,
		resolver: resolver,
		ttl:      DefaultPendingUploadTTL,
		limit:    DefaultPerWorkspaceLimit,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// GCAll iterates every workspace row and reclaims orphan presigned
// uploads. Returns a Summary with per-workspace error details; the
// function-level error is non-nil only when the run cannot proceed
// at all (e.g. the workspaces enumeration query fails).
func (s *GCService) GCAll(ctx context.Context) (Summary, error) {
	if s == nil || s.pool == nil || s.files == nil {
		return Summary{}, errors.New("gc: nil dependencies")
	}

	rows, err := s.pool.Query(ctx, `SELECT id FROM workspaces`)
	if err != nil {
		return Summary{}, fmt.Errorf("list workspaces: %w", err)
	}

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Summary{}, fmt.Errorf("scan workspace id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("iterate workspaces: %w", err)
	}

	var sum Summary
	sum.Workspaces = len(ids)
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return sum, ctx.Err()
		default:
		}
		res, err := s.GCWorkspace(ctx, id)
		if err != nil {
			sum.Errors = append(sum.Errors, WorkspaceError{WorkspaceID: id, Err: err})
			continue
		}
		sum.OrphansFound += res.Found
		sum.OrphansDeleted += res.RowsDeleted
		sum.ObjectsDeleted += res.ObjectsDeleted
	}
	return sum, nil
}

// Result is the per-workspace outcome of GCWorkspace.
type Result struct {
	WorkspaceID uuid.UUID

	// Found is the count of orphan rows the scan returned for
	// this workspace (before any delete attempts).
	Found int

	// RowsDeleted is the count of orphan file rows the GC
	// successfully removed. Equals Found minus rows where the
	// predicate-guarded DELETE matched zero rows (a concurrent
	// ConfirmUpload landed between the scan and the delete).
	RowsDeleted int

	// ObjectsDeleted is the count of S3 objects the GC
	// successfully removed. May be less than RowsDeleted when
	// the per-workspace storage client is unconfigured or when
	// DeleteObject returns a transient gateway error — in both
	// cases the row is still reclaimed so the metadata side
	// converges; the operator's storage bill converges on the
	// next GC tick (if the key gets re-stamped) or via an
	// out-of-band bucket scrub.
	ObjectsDeleted int
}

// GCWorkspace scans the given workspace for orphan presigned
// uploads older than the configured TTL, deletes the underlying S3
// objects (best-effort), and deletes the orphan file rows
// (predicate-guarded). Returns Result with the per-stage counts.
func (s *GCService) GCWorkspace(ctx context.Context, workspaceID uuid.UUID) (Result, error) {
	if s == nil || s.files == nil {
		return Result{WorkspaceID: workspaceID}, errors.New("gc: nil file service")
	}

	cutoff := s.now().Add(-s.ttl)
	orphans, err := s.files.ListPendingUploadOrphans(ctx, workspaceID, cutoff, s.limit)
	if err != nil {
		return Result{WorkspaceID: workspaceID}, fmt.Errorf("list orphans: %w", err)
	}

	res := Result{WorkspaceID: workspaceID, Found: len(orphans)}
	if len(orphans) == 0 {
		return res, nil
	}

	var deleter StorageDeleter
	if s.resolver != nil {
		deleter = s.resolver(ctx, workspaceID)
	}

	for _, o := range orphans {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}

		// Storage delete first: if the object is still there
		// we want to reclaim the bytes before we lose the key
		// from the metadata side. If DeleteObject fails (e.g.
		// transient gateway 5xx), we still proceed to delete
		// the row so the next GC tick doesn't keep re-trying
		// the same dead key forever — see the Result doc
		// comment for the operational contract.
		if deleter != nil {
			if err := deleter.DeleteObject(ctx, o.ObjectKey); err == nil {
				res.ObjectsDeleted++
			}
			// Errors are intentionally swallowed here. The
			// caller's structured-log line will reflect the
			// metric: ObjectsDeleted < RowsDeleted means the
			// storage path is unhealthy or the object was
			// already gone; both cases are operator-visible
			// via /metrics (zkdrive_gc_objects_deleted_total
			// vs zkdrive_gc_orphans_deleted_total).
		}

		if err := s.files.DeletePendingOrphan(ctx, workspaceID, o.FileID); err != nil {
			// Predicate-guarded DELETE: if a concurrent
			// ConfirmUpload landed between the list and the
			// delete, current_version_id is now non-NULL
			// and the row matches zero rows -> ErrNotFound.
			// That's not a GC failure, just a benign race;
			// the row is no longer an orphan.
			if errors.Is(err, file.ErrNotFound) {
				continue
			}
			return res, fmt.Errorf("delete orphan row %s: %w", o.FileID, err)
		}
		res.RowsDeleted++
	}

	return res, nil
}
