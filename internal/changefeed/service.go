package changefeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/logging"
)

// MaxLimit is the upper bound on the catch-up Since page size.
// 500 is comfortably below the 16 MB Postgres row-fetch ceiling for
// rows of this size and small enough that a single JSON response
// payload stays under ~1 MB. The service clamps caller-supplied
// limits to this value.
const MaxLimit = 500

// DefaultLimit is the page size used when callers pass limit <= 0.
// Sized to fit on a single screen of a sync-status UI.
const DefaultLimit = 100

// Service is the public entrypoint to the change feed. It writes
// new mutations synchronously, broadcasts them workspace-wide over
// the WebSocket hub, and serves cursor-paged catch-up reads.
//
// Errors from the publisher are intentionally non-fatal: the
// durable change_log row has already been written, so a transient
// WS / Redis outage means live clients miss a frame but every
// reconnect-and-replay completes correctly. We log and continue.
type Service struct {
	repo               Repository
	pub                WSPublisher
	cacheBuster        CacheBuster
	contentCacheBuster CacheBuster
}

// CacheBuster is the optional hook the changefeed calls after a
// mutation has been durably persisted so application-level caches
// (today: the permission cache; future: listing caches) can
// invalidate any entries the mutation might have stale-ified.
//
// Implementations MUST be cheap and fail-soft: the changefeed
// service has already returned the mutation as the durable
// authority, and a bust failure must not affect the API
// response. The CachedRepository's bust path satisfies this
// contract (logs + counter, never returns an error). The
// changefeed deliberately calls Bust without an error channel
// for that reason.
//
// BustWorkspace is the only method on the contract because the
// permission cache invalidates by workspace generation, not by
// individual resource. A finer-grained interface would be
// invoked here without changing the call site if the cache
// implementation ever moves to per-resource keying.
type CacheBuster interface {
	BustWorkspace(ctx context.Context, workspaceID uuid.UUID)
}

// NewService returns a Service backed by repo. The publisher is
// optional; pass it via WithPublisher when wiring up the server.
// Tests usually construct a Service without a publisher.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// WithPublisher attaches a WebSocket publisher. Safe to call once
// during wiring. Nil disables the live push but leaves the catch-up
// REST endpoint fully functional.
func (s *Service) WithPublisher(pub WSPublisher) *Service {
	s.pub = pub
	return s
}

// WithCacheBuster attaches a CacheBuster hook. Safe to call once
// during wiring. Nil leaves the bust path disabled — the caching
// layer then relies entirely on per-entry TTL for invalidation,
// which is still correct (just slower to converge) for any
// mutation made through the service layer.
//
// The Bust call fires AFTER the durable change_log row has been
// committed and BEFORE the WebSocket broadcast — that ordering is
// deliberate: a sync client whose live event arrives without an
// already-invalidated cache would briefly serve stale answers on
// the immediate refetch. By busting first, the cache is empty by
// the time the broadcast lands.
func (s *Service) WithCacheBuster(b CacheBuster) *Service {
	s.cacheBuster = b
	return s
}

// WithContentCacheBuster attaches a CacheBuster for *content* response
// caches (folder listings, search results). Unlike the permission cache
// — which only needs invalidation on the topology / grant mutations
// captured by shouldBustForMutation — a content cache reflects the exact
// set of files and folders in a workspace, so it must invalidate on
// EVERY recorded mutation (create, rename, delete, move, content
// re-index). This buster is therefore fired unconditionally for every
// mutation the changefeed records, not gated by shouldBustForMutation.
//
// Same fail-soft contract as WithCacheBuster: BustWorkspace must never
// error or block. Nil (or a typed-nil cache) disables it, leaving the
// content caches to converge via their per-entry TTL.
func (s *Service) WithContentCacheBuster(b CacheBuster) *Service {
	s.contentCacheBuster = b
	return s
}

// shouldBustForMutation reports whether a particular (kind, op)
// pair affects an entry the permission cache may be holding. The
// cache resolves access checks via direct grants AND the folder
// ancestry walk, so:
//
//   - permission.*: always bust. ANY grant-table mutation by
//     definition changes access resolution, so we accept broad
//     coverage rather than enumerate ops. The expected emit-set
//     today is {create,update,delete}; ops we don't emit yet
//     (rename, move) would still mean "the grant lattice
//     changed" if they ever fire. The cost of a spurious bust
//     on a future op is one INCR; the cost of MISSING a bust
//     on a future op we forgot to add is a stale-cache
//     correctness bug. The asymmetry is deliberate.
//   - folder.{move,delete}: always bust. The ancestry chain
//     changes; descendant grants resolve differently.
//   - folder.{create,update,rename}: don't bust. A new empty
//     folder has no grants to bust; a rename doesn't shift the
//     ancestor chain.
//   - file.{move}: bust. The file's ancestor chain changes.
//   - file.{create,update,rename,delete}: don't bust. Pure
//     content / metadata changes don't affect access resolution
//     on the file itself unless they're a delete, in which case
//     there are no descendants and the file's own cache entries
//     will TTL out within seconds.
//   - document.*: don't bust. Documents are a different resource
//     family that doesn't participate in the permission cache.
//
// New (kind, op) combinations are conservative-no-bust by
// default (they reach the default branch which returns false)
// so adding a new kind without updating this function would
// silently disable the cache hook for it. That failure mode
// (rely on TTL) is safer than over-busting and turning the
// cache into a churn machine, but it is still a correctness
// regression if the new kind affects permission resolution.
//
// The audit obligation is therefore enforced at test-time, not
// just by this comment:
// TestShouldBustForMutation_ExhaustivelyAuditsKindOpMatrix in
// service_test.go iterates every (Kind*, Op*) product and
// asserts the explicit decision recorded in
// knownKindOpBustDecisions matches the function's behaviour.
// The companion expectedKindCount sentinel trips CI when a new
// Kind constant is added without updating the audit ledger.
//
// Workflow when adding a new Kind:
//  1. Add the const in internal/changefeed/changefeed.go.
//  2. Bump expectedKindCount in service_test.go.
//  3. Add the new Kind to knownKinds in service_test.go.
//  4. Add bust=true|false entries for every Op in
//     knownKindOpBustDecisions, with a doc-comment justifying
//     each "false" decision.
//  5. If any decision is bust=true, add the case below.
func shouldBustForMutation(kind, op string) bool {
	switch kind {
	case KindPermission:
		return true
	case KindFolder:
		return op == OpMove || op == OpDelete
	case KindFile:
		return op == OpMove
	}
	return false
}

// RecordInput is the caller-facing input for Service.Record. It is
// a separate struct (rather than the persisted Mutation) so the
// service can fill in Sequence + OccurredAt without callers
// reasoning about which fields are server-assigned.
//
// Metadata is an arbitrary map serialised to JSONB. Keep it small
// — change_log is read by every sync client on every reconnect, so
// per-row payload bloat translates to bandwidth multiplied by
// (clients * reconnects). Callers should put only the data a sync
// client actually needs (e.g. previous name on rename, source/
// destination folder ids on move).
type RecordInput struct {
	WorkspaceID uuid.UUID
	ActorID     *uuid.UUID
	Kind        string
	Op          string
	ResourceID  uuid.UUID
	ParentID    *uuid.UUID
	Name        string
	Metadata    map[string]any
}

// validate enforces the CHECK constraints on Kind/Op in code so a
// caller's typo (Kind = "files" instead of "file") fails fast with
// a useful error rather than tripping a Postgres constraint
// violation that has to be parsed out of pgconn.PgError.
func (in *RecordInput) validate() error {
	switch in.Kind {
	case KindFile, KindFolder, KindPermission, KindDocument:
	default:
		return fmt.Errorf("changefeed: invalid kind %q", in.Kind)
	}
	switch in.Op {
	case OpCreate, OpUpdate, OpRename, OpMove, OpDelete:
	default:
		return fmt.Errorf("changefeed: invalid op %q", in.Op)
	}
	if in.WorkspaceID == uuid.Nil {
		return errors.New("changefeed: workspace_id is required")
	}
	if in.ResourceID == uuid.Nil {
		return errors.New("changefeed: resource_id is required")
	}
	return nil
}

// Record persists a Mutation and broadcasts it. Returns the
// fully-populated Mutation (with Sequence + OccurredAt) so callers
// that want to surface the resulting row in an HTTP response can do
// so without an extra query.
//
// The persist + broadcast pair is intentionally NOT atomic: the
// durable row is the source of truth. If broadcast fails (Redis
// outage, hub overloaded) the row is still in the log, and any
// reconnecting client will replay it via Since(). The reverse —
// broadcasting a mutation that wasn't persisted — would be a
// correctness bug, which is why Record is sequenced repo-first.
func (s *Service) Record(ctx context.Context, in RecordInput) (Mutation, error) {
	if err := in.validate(); err != nil {
		return Mutation{}, err
	}
	var metadata json.RawMessage
	if len(in.Metadata) > 0 {
		raw, err := json.Marshal(in.Metadata)
		if err != nil {
			return Mutation{}, fmt.Errorf("marshal metadata: %w", err)
		}
		metadata = raw
	}
	m := Mutation{
		WorkspaceID: in.WorkspaceID,
		ActorID:     in.ActorID,
		Kind:        in.Kind,
		Op:          in.Op,
		ResourceID:  in.ResourceID,
		ParentID:    in.ParentID,
		Name:        in.Name,
		Metadata:    metadata,
	}
	if err := s.repo.Record(ctx, &m); err != nil {
		return Mutation{}, err
	}
	// Bust the application-level cache BEFORE the WS broadcast
	// fires so a client that reacts to the live event (e.g. by
	// re-fetching the affected resource) sees the post-mutation
	// state, not a stale cached one. Bust failures are
	// already logged + counted inside the implementation and
	// never returned, matching the publisher's best-effort
	// semantics — durability lives in change_log, the cache is
	// a perf overlay.
	if s.cacheBuster != nil && shouldBustForMutation(in.Kind, in.Op) {
		s.cacheBuster.BustWorkspace(ctx, in.WorkspaceID)
	}
	// Content caches (listing/search) reflect the exact resource set,
	// so any recorded mutation invalidates them — no predicate gate.
	if s.contentCacheBuster != nil {
		s.contentCacheBuster.BustWorkspace(ctx, in.WorkspaceID)
	}
	if s.pub != nil {
		if err := s.pub.Publish(ctx, in.WorkspaceID, Event{Type: "change", Payload: m}); err != nil {
			logging.FromContext(ctx).Warn("changefeed publish failed; row persisted",
				"workspace_id", in.WorkspaceID,
				"sequence", m.Sequence,
				"err", err,
			)
		}
	}
	return m, nil
}

// BatchRecord persists a slice of RecordInputs in a single multi-row
// INSERT and broadcasts each resulting Mutation. Bulk handlers
// (move/copy/delete of N items) use this to amortise the per-row
// round-trip cost across the entire bulk operation while preserving
// the durability guarantee that the catch-up cursor advances before
// the HTTP response returns.
//
// Validation, metadata marshalling, and broadcasting mirror the
// single-item Record path so callers see identical semantics. An
// empty slice is a no-op (returns nil, nil). Inputs that fail
// validation cause the entire batch to fail before any DB write:
// partial-success would leak gaps into the cursor stream which is
// worse than an outright error for clients.
//
// Each event is published independently because the WS payload is
// one mutation per envelope — the existing client protocol does
// not understand a "batched" payload and changing it would force a
// SDK upgrade. The publishes happen sequentially in sequence order
// so clients observing the live stream see the same order as
// clients catching up via Since.
func (s *Service) BatchRecord(ctx context.Context, inputs []RecordInput) ([]Mutation, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	muts := make([]Mutation, len(inputs))
	for i := range inputs {
		if err := inputs[i].validate(); err != nil {
			return nil, fmt.Errorf("batch[%d]: %w", i, err)
		}
		var metadata json.RawMessage
		if len(inputs[i].Metadata) > 0 {
			raw, err := json.Marshal(inputs[i].Metadata)
			if err != nil {
				return nil, fmt.Errorf("batch[%d]: marshal metadata: %w", i, err)
			}
			metadata = raw
		}
		muts[i] = Mutation{
			WorkspaceID: inputs[i].WorkspaceID,
			ActorID:     inputs[i].ActorID,
			Kind:        inputs[i].Kind,
			Op:          inputs[i].Op,
			ResourceID:  inputs[i].ResourceID,
			ParentID:    inputs[i].ParentID,
			Name:        inputs[i].Name,
			Metadata:    metadata,
		}
	}
	if err := s.repo.BatchRecord(ctx, muts); err != nil {
		return nil, err
	}
	// De-duplicate bust calls across the batch by workspace —
	// a 100-item bulk-move emits 100 muts all targeting the
	// same workspace, but a single bust per workspace is
	// enough to invalidate everything (the generation counter
	// is workspace-scoped). Without de-duplication a 1000-item
	// bulk would issue 1000 redundant INCRs.
	if s.cacheBuster != nil {
		seen := make(map[uuid.UUID]struct{}, len(muts))
		for _, m := range muts {
			if !shouldBustForMutation(m.Kind, m.Op) {
				continue
			}
			if _, dup := seen[m.WorkspaceID]; dup {
				continue
			}
			seen[m.WorkspaceID] = struct{}{}
			s.cacheBuster.BustWorkspace(ctx, m.WorkspaceID)
		}
	}
	// Content caches invalidate on every mutation; de-dup per workspace
	// so a 1000-item bulk issues one INCR per affected workspace.
	if s.contentCacheBuster != nil {
		seen := make(map[uuid.UUID]struct{}, len(muts))
		for _, m := range muts {
			if _, dup := seen[m.WorkspaceID]; dup {
				continue
			}
			seen[m.WorkspaceID] = struct{}{}
			s.contentCacheBuster.BustWorkspace(ctx, m.WorkspaceID)
		}
	}
	if s.pub != nil {
		for _, m := range muts {
			if err := s.pub.Publish(ctx, m.WorkspaceID, Event{Type: "change", Payload: m}); err != nil {
				logging.FromContext(ctx).Warn("changefeed batch publish failed; row persisted",
					"workspace_id", m.WorkspaceID,
					"sequence", m.Sequence,
					"err", err,
				)
			}
		}
	}
	return muts, nil
}

// Since returns the next page of mutations for `workspaceID` strictly
// after `cursor`. The returned Page contains an advanced cursor that
// callers should pass on the next call. Limit is clamped to (0,
// MaxLimit] before being forwarded to the repository.
func (s *Service) Since(ctx context.Context, workspaceID uuid.UUID, cursor int64, limit int) (Page, error) {
	if workspaceID == uuid.Nil {
		return Page{}, errors.New("changefeed: workspace_id is required")
	}
	if cursor < 0 {
		cursor = 0
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	muts, hasMore, err := s.repo.Since(ctx, workspaceID, cursor, limit)
	if err != nil {
		return Page{}, err
	}
	advanced := cursor
	if len(muts) > 0 {
		advanced = muts[len(muts)-1].Sequence
	}
	return Page{
		Mutations: muts,
		Cursor:    advanced,
		HasMore:   hasMore,
	}, nil
}

// Latest returns the highest sequence currently stored for the
// workspace. Sync clients use this on first connect to learn the
// "now" cursor before they start receiving live events — they store
// the value, then any incoming live event with a higher sequence is
// processed and any reconnect after a disconnect uses it as the
// `since` cursor.
func (s *Service) Latest(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if workspaceID == uuid.Nil {
		return 0, errors.New("changefeed: workspace_id is required")
	}
	return s.repo.Latest(ctx, workspaceID)
}
