package audit

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// defaultBufferSize is the capacity of the internal entries channel.
const defaultBufferSize = 1024

// defaultFlushTimeout bounds each INSERT so a stalled Postgres
// connection can't pile up goroutines inside the worker.
const defaultFlushTimeout = 5 * time.Second

// Service is a fire-and-forget audit logger. It mirrors the activity
// service pattern: Log copies the entry onto a buffered channel and
// returns immediately; a background goroutine drains the channel and
// writes to audit_log. Failures are logged and swallowed so they
// never fail the parent operation.
type Service struct {
	repo    Repository
	entries chan *Entry
	wg      sync.WaitGroup
	once    sync.Once
	closed  chan struct{}
	timeout time.Duration
}

// NewService returns a Service backed by the given repository and
// starts the background worker. Callers own the lifetime and must
// invoke Close before shutting the process down.
func NewService(repo Repository) *Service {
	s := &Service{
		repo:    repo,
		entries: make(chan *Entry, defaultBufferSize),
		closed:  make(chan struct{}),
		timeout: defaultFlushTimeout,
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Log enqueues an entry for async persistence. It never blocks: when
// the buffer is full or Close has fired, the entry is dropped and a
// warning is logged.
func (s *Service) Log(_ context.Context, entry *Entry) {
	if entry == nil {
		return
	}
	select {
	case <-s.closed:
		return
	case s.entries <- entry:
	default:
		log.Printf("audit: buffer full, dropping entry action=%s", entry.Action)
	}
}

// LogAction is a convenience for callers that don't need to construct
// an Entry literal. metadata may be nil.
func (s *Service) LogAction(
	ctx context.Context,
	workspaceID uuid.UUID,
	actorID *uuid.UUID,
	action string,
	resourceType string,
	resourceID *uuid.UUID,
	r *http.Request,
	metadata map[string]any,
) {
	var raw json.RawMessage
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			log.Printf("audit: marshal metadata: %v", err)
		} else {
			raw = b
		}
	}
	entry := &Entry{
		WorkspaceID: workspaceID,
		ActorID:     actorID,
		Action:      action,
		Metadata:    raw,
	}
	if resourceType != "" {
		rt := resourceType
		entry.ResourceType = &rt
	}
	if resourceID != nil {
		rid := *resourceID
		entry.ResourceID = &rid
	}
	if r != nil {
		if ip := clientIP(r); ip != "" {
			entry.IPAddress = &ip
		}
		if ua := r.UserAgent(); ua != "" {
			entry.UserAgent = &ua
		}
	}
	s.Log(ctx, entry)
}

// List proxies to the repository. Reads are synchronous because the
// paginated fetch is the operation the caller asked for.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID, action string, limit, offset int) ([]*Entry, error) {
	return s.repo.List(ctx, workspaceID, action, limit, offset)
}

// Close drains pending entries and stops the worker. Safe to call
// more than once.
func (s *Service) Close() {
	s.once.Do(func() {
		close(s.closed)
	})
	s.wg.Wait()
}

func (s *Service) run() {
	defer s.wg.Done()
	for {
		select {
		case entry := <-s.entries:
			s.flush(entry)
		case <-s.closed:
			for {
				select {
				case entry := <-s.entries:
					s.flush(entry)
				default:
					return
				}
			}
		}
	}
}

func (s *Service) flush(entry *Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if err := s.repo.Log(ctx, entry); err != nil {
		log.Printf("audit: persist failed action=%s err=%v", entry.Action, err)
	}
}

// clientIP extracts the request's client IP. Chi's RealIP middleware
// rewrites r.RemoteAddr when X-Forwarded-For is present, so stripping
// the port suffix from r.RemoteAddr is sufficient for live traffic.
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if addr == "" {
		return ""
	}
	// Strip port if present (handles both "1.2.3.4:5" and
	// "[::1]:5" forms).
	if i := strings.LastIndex(addr, ":"); i > 0 {
		host := addr[:i]
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		return host
	}
	return addr
}
