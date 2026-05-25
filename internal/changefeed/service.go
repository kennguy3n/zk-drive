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
	repo Repository
	pub  WSPublisher
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
