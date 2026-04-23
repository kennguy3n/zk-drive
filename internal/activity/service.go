package activity

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// defaultBufferSize is the capacity of the internal entries channel. At
// burst this many log writes can queue before Log() starts dropping.
const defaultBufferSize = 1024

// defaultFlushTimeout bounds each INSERT so a stalled Postgres connection
// can't pile up goroutines inside the worker.
const defaultFlushTimeout = 5 * time.Second

// Service is a fire-and-forget activity logger. Log() copies the entry
// onto an internal buffered channel and returns immediately; a background
// goroutine drains the channel and writes to activity_log. Failures are
// logged and swallowed so they never fail the parent operation. Callers
// must invoke Close before shutting the process down to drain in-flight
// entries.
type Service struct {
	repo    Repository
	entries chan *LogEntry
	wg      sync.WaitGroup
	once    sync.Once
	closed  chan struct{}
	timeout time.Duration
}

// NewService returns a Service backed by the given repository and starts
// the background worker. The caller owns the lifetime and must call Close.
func NewService(repo Repository) *Service {
	s := &Service{
		repo:    repo,
		entries: make(chan *LogEntry, defaultBufferSize),
		closed:  make(chan struct{}),
		timeout: defaultFlushTimeout,
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Log enqueues an entry for asynchronous persistence. It never blocks: if
// the internal buffer is full, the entry is dropped and a warning is
// logged. Errors in persistence are logged but never surfaced to the
// caller, per the fire-and-forget contract.
func (s *Service) Log(_ context.Context, entry *LogEntry) {
	if entry == nil {
		return
	}
	select {
	case <-s.closed:
		return
	default:
	}
	select {
	case s.entries <- entry:
	default:
		log.Printf("activity: buffer full, dropping entry action=%s resource=%s/%s",
			entry.Action, entry.ResourceType, entry.ResourceID)
	}
}

// LogAction is a convenience for callers that don't need to build a LogEntry
// explicitly. metadata may be nil.
func (s *Service) LogAction(ctx context.Context, workspaceID, userID uuid.UUID, action, resourceType string, resourceID uuid.UUID, metadata map[string]any) {
	var raw json.RawMessage
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			log.Printf("activity: marshal metadata: %v", err)
		} else {
			raw = b
		}
	}
	s.Log(ctx, &LogEntry{
		WorkspaceID:  workspaceID,
		UserID:       userID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		MetadataJSON: raw,
	})
}

// List proxies to the repository. Reads are synchronous because the
// paginated fetch is itself the operation the caller asked for.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID, limit, offset int) ([]*LogEntry, error) {
	return s.repo.List(ctx, workspaceID, limit, offset)
}

// ListByResource proxies to the repository.
func (s *Service) ListByResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, limit, offset int) ([]*LogEntry, error) {
	return s.repo.ListByResource(ctx, workspaceID, resourceType, resourceID, limit, offset)
}

// Close drains any pending entries and stops the worker. Safe to call more
// than once.
func (s *Service) Close() {
	s.once.Do(func() {
		close(s.closed)
		close(s.entries)
	})
	s.wg.Wait()
}

func (s *Service) run() {
	defer s.wg.Done()
	for entry := range s.entries {
		s.flush(entry)
	}
}

func (s *Service) flush(entry *LogEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if err := s.repo.Log(ctx, entry); err != nil {
		log.Printf("activity: persist failed action=%s resource=%s/%s err=%v",
			entry.Action, entry.ResourceType, entry.ResourceID, err)
	}
}
