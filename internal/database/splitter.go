package database

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of *pgxpool.Pool that the repository layer
// consumes. Both *pgxpool.Pool and *ReadWriteSplitter satisfy it, so a
// repository can be wired against either a single pool (the historical
// behaviour) or a read/write-split pair WITHOUT any call-site change:
// *pgxpool.Pool already implements every method here, so changing a
// repository field from *pgxpool.Pool to Querier is source-compatible.
//
// The interface deliberately omits pool lifecycle methods (Close, Stat,
// Ping, Acquire). Those operate on a concrete pool and have no sensible
// "route by query kind" semantics; callers that need them hold the
// concrete *pgxpool.Pool (the primary) directly.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	Begin(ctx context.Context) (pgx.Tx, error)
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

// Compile-time assertions that both concrete types satisfy Querier.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (*ReadWriteSplitter)(nil)
)

// ReadWriteSplitter routes read-only statements to a replica pool and
// every mutation (plus anything it cannot prove is read-only) to the
// primary pool. It implements Querier so it is a drop-in replacement
// for a *pgxpool.Pool in any repository.
//
// Routing policy (correctness-first):
//   - Query / QueryRow: routed to the replica ONLY when the SQL text is
//     classified read-only by isReadOnlySQL (a plain SELECT / WITH-only
//     CTE / VALUES / TABLE / EXPLAIN-without-ANALYZE / SHOW), AND the
//     statement carries no row-locking clause (FOR UPDATE / SHARE) and
//     no data-modifying CTE. Anything else falls through to the primary.
//   - Exec, Begin, BeginTx, SendBatch, CopyFrom: ALWAYS routed to the
//     primary. Exec is overwhelmingly used for INSERT/UPDATE/DELETE/DDL;
//     batches and transactions can interleave writes; CopyFrom is a bulk
//     write. Sending any of these to a read replica would either error
//     (read-only transaction) or, worse for a writeable replica, split a
//     logical unit of work across hosts.
//
// When replica == primary (the no-replica deployment), every method is a
// straight pass-through to the single pool and the classifier is skipped
// for Exec/Begin paths, so the splitter adds no measurable overhead.
//
// Replica lag note: a SELECT issued immediately after a write on the same
// logical entity may observe a slightly stale replica. Callers that need
// read-your-write consistency must run inside a transaction (Begin →
// primary) or use a *pgxpool.Pool directly. This is the standard
// trade-off of physical read replicas and is documented in
// docs/CONFIGURATION.md.
type ReadWriteSplitter struct {
	primary *pgxpool.Pool
	replica *pgxpool.Pool
}

// NewReadWriteSplitter builds a splitter over a primary and replica
// pool. A nil replica is treated as "no replica configured": the
// primary is used for reads too, so the splitter degrades to a plain
// single-pool wrapper. primary must be non-nil.
func NewReadWriteSplitter(primary, replica *pgxpool.Pool) *ReadWriteSplitter {
	if primary == nil {
		panic("database: NewReadWriteSplitter requires a non-nil primary pool")
	}
	if replica == nil {
		replica = primary
	}
	return &ReadWriteSplitter{primary: primary, replica: replica}
}

// ConnectReadWrite opens the primary pool from primaryDSN and, when
// readDSN is non-empty and distinct, a separate replica pool from
// readDSN, returning a splitter over the pair. Both pools are created
// with ConnectWithPool so they share the tenant-GUC bind hook, the
// OTel tracer, and the supplied sizing.
//
// readDSN semantics:
//   - empty / whitespace: no replica pool is opened; reads use primary.
//   - identical to primaryDSN: no second pool (it would just double the
//     connection count against the same host); reads use primary.
//   - otherwise: a second pool is opened and used for routed reads.
//
// On any failure the already-opened primary pool is closed before
// returning so the caller never leaks a pool on the error path.
func ConnectReadWrite(ctx context.Context, primaryDSN, readDSN string, pc PoolConfig) (*ReadWriteSplitter, error) {
	primary, err := ConnectWithPool(ctx, primaryDSN, pc)
	if err != nil {
		return nil, fmt.Errorf("connect primary: %w", err)
	}
	readDSN = strings.TrimSpace(readDSN)
	if readDSN == "" || readDSN == strings.TrimSpace(primaryDSN) {
		return NewReadWriteSplitter(primary, nil), nil
	}
	replica, err := ConnectWithPool(ctx, readDSN, pc)
	if err != nil {
		primary.Close()
		return nil, fmt.Errorf("connect read replica: %w", err)
	}
	return NewReadWriteSplitter(primary, replica), nil
}

// Primary returns the underlying primary pool for callers that need the
// concrete type — pool lifecycle (Close, Stat), migrations (which take a
// *pgxpool.Pool and must run against the primary), and any read path
// that explicitly requires read-your-write consistency.
func (s *ReadWriteSplitter) Primary() *pgxpool.Pool { return s.primary }

// Replica returns the pool reads are routed to. Equals Primary() when no
// replica is configured. Exposed for health checks / metrics that want
// to ping the read path independently.
func (s *ReadWriteSplitter) Replica() *pgxpool.Pool { return s.replica }

// HasReplica reports whether a distinct replica pool is in use.
func (s *ReadWriteSplitter) HasReplica() bool { return s.replica != s.primary }

// Close closes both pools. The replica is closed first; when no replica
// is configured (replica == primary) it is closed exactly once.
func (s *ReadWriteSplitter) Close() {
	if s.replica != s.primary {
		s.replica.Close()
	}
	s.primary.Close()
}

// Ping pings both pools so a startup / health probe surfaces a degraded
// replica. A nil error means both the primary and the replica answered.
func (s *ReadWriteSplitter) Ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.primary.Ping(pctx); err != nil {
		return fmt.Errorf("ping primary: %w", err)
	}
	if s.replica != s.primary {
		if err := s.replica.Ping(pctx); err != nil {
			return fmt.Errorf("ping replica: %w", err)
		}
	}
	return nil
}

// Query routes to the replica when the SQL is read-only, else primary.
func (s *ReadWriteSplitter) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.readPool(sql).Query(ctx, sql, args...)
}

// QueryRow routes to the replica when the SQL is read-only, else primary.
func (s *ReadWriteSplitter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.readPool(sql).QueryRow(ctx, sql, args...)
}

// Exec always targets the primary (see type doc).
func (s *ReadWriteSplitter) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return s.primary.Exec(ctx, sql, arguments...)
}

// SendBatch always targets the primary (a batch may interleave writes).
func (s *ReadWriteSplitter) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return s.primary.SendBatch(ctx, b)
}

// Begin always targets the primary.
func (s *ReadWriteSplitter) Begin(ctx context.Context) (pgx.Tx, error) {
	return s.primary.Begin(ctx)
}

// BeginTx always targets the primary, even for pgx.ReadOnly transactions:
// a transaction is a single unit of work that may follow a read with a
// write, and splitting it across hosts is never correct.
func (s *ReadWriteSplitter) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return s.primary.BeginTx(ctx, txOptions)
}

// CopyFrom always targets the primary (bulk write).
func (s *ReadWriteSplitter) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return s.primary.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

// readPool picks the pool a SELECT-family statement should run on.
func (s *ReadWriteSplitter) readPool(sql string) *pgxpool.Pool {
	if s.replica != s.primary && isReadOnlySQL(sql) {
		return s.replica
	}
	return s.primary
}

// isReadOnlySQL reports whether sql is safe to route to a read replica.
// It is intentionally conservative: it returns true only for statements
// it can positively classify as non-mutating, and false (→ primary) for
// anything ambiguous. False negatives only cost a missed read-offload;
// a false positive could route a write to a read-only replica and fail
// the request, so the asymmetry is deliberate.
//
// Recognised read-only leading keywords: SELECT, WITH (recursive CTE),
// VALUES, TABLE, SHOW, EXPLAIN (without ANALYZE). Any statement whose
// text contains a data-modifying keyword (INSERT/UPDATE/DELETE/MERGE)
// — as happens with a writeable CTE like `WITH x AS (UPDATE ...)` — or a
// row-locking clause (FOR UPDATE / FOR NO KEY UPDATE / FOR SHARE / FOR
// KEY SHARE) is forced to the primary.
func isReadOnlySQL(sql string) bool {
	s := stripLeadingSQLNoise(sql)
	if s == "" {
		return false
	}
	upper := strings.ToUpper(s)

	var lead string
	switch {
	case strings.HasPrefix(upper, "SELECT"):
		lead = "SELECT"
	case strings.HasPrefix(upper, "WITH"):
		lead = "WITH"
	case strings.HasPrefix(upper, "VALUES"):
		lead = "VALUES"
	case strings.HasPrefix(upper, "TABLE"):
		lead = "TABLE"
	case strings.HasPrefix(upper, "SHOW"):
		return true // SHOW carries no write risk; short-circuit.
	case strings.HasPrefix(upper, "EXPLAIN"):
		// EXPLAIN ANALYZE actually executes the plan (and thus a
		// nested INSERT/UPDATE would mutate), so only a plain
		// EXPLAIN is replica-safe.
		return !strings.Contains(upper, "ANALYZE")
	default:
		return false
	}
	// Guard the keyword boundary: a statement like "SELECTOR" must not
	// match SELECT. The first char after the keyword must be a space or
	// '(' (e.g. "WITH(" is invalid SQL, but "SELECT(" never occurs;
	// being strict here is harmless).
	if len(s) > len(lead) {
		next := rune(s[len(lead)])
		if !unicode.IsSpace(next) && next != '(' {
			return false
		}
	}

	// A writeable CTE or any embedded DML forces the primary. We scan
	// the whole (uppercased) statement for word-boundaried mutation
	// keywords. This is a superset check: a SELECT that merely mentions
	// the word "UPDATE" inside a string literal or column alias would be
	// conservatively routed to the primary, which is safe.
	for _, kw := range writeKeywords {
		if containsWord(upper, kw) {
			return false
		}
	}
	return true
}

// writeKeywords are the tokens whose presence anywhere in a statement
// disqualifies it from replica routing.
var writeKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "MERGE",
	"FOR UPDATE", "FOR NO KEY UPDATE", "FOR SHARE", "FOR KEY SHARE",
	"NEXTVAL", "SETVAL", // sequence mutation via SELECT nextval(...)
}

// stripLeadingSQLNoise removes leading whitespace and SQL line/block
// comments so the leading-keyword check sees the first real token. It
// only strips from the FRONT; comments deeper in the statement are left
// for the word scan (they cannot introduce a write).
func stripLeadingSQLNoise(sql string) string {
	s := strings.TrimSpace(sql)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			if idx := strings.IndexByte(s, '\n'); idx >= 0 {
				s = strings.TrimSpace(s[idx+1:])
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if idx := strings.Index(s, "*/"); idx >= 0 {
				s = strings.TrimSpace(s[idx+2:])
				continue
			}
			return ""
		default:
			return s
		}
	}
}

// containsWord reports whether word appears in s delimited by
// non-alphanumeric boundaries (so "UPDATE" matches in "FOR UPDATE x" but
// not in "UPDATED_AT"). s and word are assumed already upper-cased.
func containsWord(s, word string) bool {
	from := 0
	for {
		idx := strings.Index(s[from:], word)
		if idx < 0 {
			return false
		}
		start := from + idx
		end := start + len(word)
		beforeOK := start == 0 || !isIdentRune(rune(s[start-1]))
		afterOK := end == len(s) || !isIdentRune(rune(s[end]))
		if beforeOK && afterOK {
			return true
		}
		from = start + 1
	}
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
