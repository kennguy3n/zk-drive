// Package search implements workspace-scoped multilingual search
// across files and folders.
//
// Strategy
//
// The search service runs TWO Postgres paths and UNIONs the result:
//
//   1. FTS path: to_tsvector(lang, immutable_unaccent(name || ...))
//      @@ plainto_tsquery(lang, immutable_unaccent($q)). Uses the
//      workspace's configured search_language for stemming so a
//      query for "running" matches stored "ran" / "runs" in
//      English-stemmed corpora. The 'simple' dictionary path
//      (default) only matches whole word tokens after accent-fold,
//      which is fine for short queries and never misses a hit the
//      trigram path would have caught.
//
//   2. Trigram path: immutable_unaccent(name || ...) <% immutable_unaccent($q)
//      with word_similarity(...) scoring. The <% operator + GIN
//      trgm index handles CJK substrings (no word boundaries
//      needed) and fuzzy matches like "reportt" → "report". We use
//      word_similarity / <% (NOT plain similarity / %) because
//      filenames often embed the query inside longer strings
//      ("季度报告.txt" contains "季度" but plain similarity is
//      diluted by the .txt suffix and the rest of the filename).
//      <% scores the BEST matching window of the longer string
//      against the shorter query — which is exactly the semantics
//      we want for filename / content_text search.
//
//      pg_trgm.word_similarity_threshold is SET LOCAL inside the
//      search transaction so the planner can use the GIN index
//      but the threshold doesn't leak across the pool.
//
// Each path independently scores its hits, then a SELECT DISTINCT
// ON over the UNION'd set keeps the better-ranked hit per (type,
// id). The total budget is `limit` rows; the inner subqueries each
// fetch limit*4 candidates so both strategies have headroom to
// dominate their stronger language families before the outer
// ranking trims to limit.
//
// Read the migration at migrations/032_search_extensions.up.sql for
// the supporting indexes / immutable_unaccent function definition.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/workspace"
)

// Result is a single hit in a search response. Type is "file" or
// "folder"; Path is the folder's materialized path (for folders) or
// the parent folder's path (for files) so the frontend can show the
// hit in context without a second round-trip. Rank is the Postgres
// score (max of ts_rank_cd and similarity, normalised to [0, 1]).
// Tags is non-nil only on file hits; folder hits always omit it.
type Result struct {
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	FolderID  uuid.UUID `json:"folder_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Rank      float32   `json:"rank"`
	Tags      []string  `json:"tags,omitempty"`
}

// Service runs workspace-scoped multilingual search queries.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service backed by the supplied pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// DefaultLimit is the page size used when the caller passes limit <= 0.
const DefaultLimit = 20

// MaxLimit caps the page size to prevent unbounded queries. Clients
// that need more can paginate with offset.
const MaxLimit = 100

// candidateMultiplier is how many extra candidate rows each strategy
// (FTS, trigram) pulls before the outer SELECT DISTINCT ON ranks
// across them. A higher multiplier improves recall when both paths
// hit the same documents (we want the better-scored copy), at the
// cost of slightly more pgsql work. 4× is a conservative starting
// point — page sizes are bounded at MaxLimit (100) so the inner
// candidate set never exceeds ~400 per strategy.
const candidateMultiplier = 4

// DefaultTrigramThreshold is the pg_trgm.word_similarity_threshold
// the service installs per-query when FuzzyEnabled is false. 0.45
// is below the Postgres default (0.6) for <% so short queries
// ("季度" = 2 CJK chars) still match longer indexed strings, but
// not so loose that unrelated single-token matches leak through.
const DefaultTrigramThreshold = 0.45

// FuzzyTrigramThreshold relaxes the similarity floor when the
// caller opts in via FuzzyEnabled=true. 0.25 catches single-char
// typos in 4-6 letter words ("reportt" → "report") and very short
// CJK substrings (1-2 chars) without drowning the result set in
// noise. Tested against a corpus of realistic English filenames
// and CJK fixtures in tests/integration/search_multilingual_test.go.
const FuzzyTrigramThreshold = 0.25

// ErrInvalidQuery is returned when the caller supplies an empty query.
var ErrInvalidQuery = errors.New("search: query is required")

// Options control optional behaviour of the Search call. Zero value
// is the recommended default: FTS dictionary is the workspace's
// search_language (resolved by the caller), fuzzy matching is off.
type Options struct {
	// Language is the Postgres text-search dictionary the FTS
	// path uses for stemming. Must be a value accepted by
	// workspace.IsSupportedSearchLanguage; the caller is
	// responsible for that validation. Empty string is treated
	// as workspace.DefaultSearchLanguage ('simple').
	Language string
	// FuzzyEnabled relaxes the trigram similarity threshold so
	// single-char typos still surface results. Exposed as a
	// per-query flag so the default search-as-you-type behaviour
	// stays tight (avoiding spurious matches on prefix typos)
	// while an explicit user toggle (?fuzzy=true) gets the
	// relaxed match.
	FuzzyEnabled bool
}

// resolvedLanguage returns the FTS dictionary the FTS path should
// use. We accept the small set workspace exports so a typo in the
// admin endpoint can't poison the query (Postgres would error on
// to_tsvector(unknown, ...) and the whole search 500s).
func (o Options) resolvedLanguage() string {
	if o.Language == "" {
		return workspace.DefaultSearchLanguage
	}
	if !workspace.IsSupportedSearchLanguage(o.Language) {
		return workspace.DefaultSearchLanguage
	}
	return o.Language
}

// Search runs the multilingual UNION query and returns at most
// `limit` hits, ordered by score descending and (for ties) by
// created_at descending. Soft-deleted rows and strict_zk folders
// are excluded.
//
// The query is executed inside a short-lived transaction so the
// SET LOCAL for pg_trgm.similarity_threshold is scoped to the
// query and never leaks back to the pool's other consumers.
func (s *Service) Search(ctx context.Context, workspaceID uuid.UUID, query string, opts Options, limit, offset int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, ErrInvalidQuery
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if offset < 0 {
		offset = 0
	}

	lang := opts.resolvedLanguage()
	threshold := DefaultTrigramThreshold
	if opts.FuzzyEnabled {
		threshold = FuzzyTrigramThreshold
	}
	candidateLimit := limit*candidateMultiplier + offset

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("search: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SET LOCAL lives for the duration of the transaction. The
	// % operator consults pg_trgm.similarity_threshold to decide
	// whether two strings are "close enough" — keeping the
	// threshold explicit per query lets the FuzzyEnabled flag
	// alter recall without an ALTER DATABASE or a daemon-level
	// GUC.
	if _, err := tx.Exec(ctx,
		fmt.Sprintf("SET LOCAL pg_trgm.word_similarity_threshold = %f", threshold),
	); err != nil {
		return nil, fmt.Errorf("search: set threshold: %w", err)
	}

	// The FTS dictionary identifier (lang) is a regconfig —
	// Postgres cannot bind it as a $N parameter, only as a literal.
	// We validated lang against the allow-list above so substituting
	// it via strings.ReplaceAll is safe (no operator-controlled
	// input can land here). Using a quoted regconfig literal
	// (::regconfig) is the documented mechanism — Postgres parses it
	// at plan time and the planner can reuse the prepared plan.
	//
	// We use a placeholder token — NOT fmt.Sprintf — because the
	// SQL contains pg_trgm's `<%` and `%>` operators which fmt's
	// vet would otherwise misinterpret as printf directives.
	q := strings.ReplaceAll(searchSQL, "__LANG__", lang)

	rows, err := tx.Query(ctx, q, workspaceID, query, candidateLimit, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	out := make([]Result, 0, limit)
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Commit the read-only tx so the SET LOCAL is cleanly released.
	// Rolling back works too but Commit signals intent and matches
	// the Postgres docs' recommendation for read-only paths.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("search: commit tx: %w", err)
	}
	return out, nil
}

// searchSQL is the dual-strategy multilingual query. __LANG__ is
// the placeholder for the workspace's regconfig dictionary; it is
// substituted at query-build time via strings.ReplaceAll after
// validation against workspace.IsSupportedSearchLanguage. See the
// package-level docstring for an overview of the FTS + trigram
// UNION strategy.
const searchSQL = `
WITH file_tag_text AS (
    SELECT file_id,
           string_agg(tag, ' ') AS tag_text,
           array_agg(DISTINCT tag ORDER BY tag) AS tags
    FROM file_tags
    WHERE workspace_id = $1
    GROUP BY file_id
),
fts_files AS (
    SELECT 'file'::TEXT AS type,
           f.id,
           f.name,
           parent.path AS path,
           f.folder_id,
           f.created_at,
           ts_rank_cd(
               to_tsvector('__LANG__'::regconfig,
                   immutable_unaccent(f.name || ' ' || COALESCE(ft.tag_text, '') || ' ' || COALESCE(f.content_text, ''))),
               plainto_tsquery('__LANG__'::regconfig, immutable_unaccent($2))
           )::float4 AS rank,
           COALESCE(ft.tags, ARRAY[]::TEXT[]) AS tags
    FROM files f
    JOIN folders parent ON parent.id = f.folder_id
    LEFT JOIN file_tag_text ft ON ft.file_id = f.id
    WHERE f.workspace_id = $1
      AND f.deleted_at IS NULL
      AND parent.deleted_at IS NULL
      AND parent.encryption_mode <> 'strict_zk'
      AND to_tsvector('__LANG__'::regconfig,
              immutable_unaccent(f.name || ' ' || COALESCE(ft.tag_text, '') || ' ' || COALESCE(f.content_text, '')))
          @@ plainto_tsquery('__LANG__'::regconfig, immutable_unaccent($2))
    ORDER BY rank DESC, f.created_at DESC
    LIMIT $3
),
fts_folders AS (
    SELECT 'folder'::TEXT AS type,
           fo.id,
           fo.name,
           fo.path AS path,
           NULL::UUID AS folder_id,
           fo.created_at,
           ts_rank_cd(
               to_tsvector('__LANG__'::regconfig, immutable_unaccent(fo.name)),
               plainto_tsquery('__LANG__'::regconfig, immutable_unaccent($2))
           )::float4 AS rank,
           ARRAY[]::TEXT[] AS tags
    FROM folders fo
    WHERE fo.workspace_id = $1
      AND fo.deleted_at IS NULL
      AND fo.encryption_mode <> 'strict_zk'
      AND to_tsvector('__LANG__'::regconfig, immutable_unaccent(fo.name))
          @@ plainto_tsquery('__LANG__'::regconfig, immutable_unaccent($2))
    ORDER BY rank DESC, fo.created_at DESC
    LIMIT $3
),
trgm_files AS (
    -- word_similarity / <% finds the BEST trigram window of the
    -- longer indexed string against the shorter query. This is
    -- the operator we want for filename + content_text search:
    -- "季度" should match "季度报告.txt" with a high score, even
    -- though plain similarity() would be diluted by the .txt
    -- suffix and the rest of the content.
    SELECT 'file'::TEXT AS type,
           f.id,
           f.name,
           parent.path AS path,
           f.folder_id,
           f.created_at,
           word_similarity(
               immutable_unaccent($2),
               immutable_unaccent(f.name || ' ' || COALESCE(ft.tag_text, '') || ' ' || COALESCE(f.content_text, ''))
           )::float4 AS rank,
           COALESCE(ft.tags, ARRAY[]::TEXT[]) AS tags
    FROM files f
    JOIN folders parent ON parent.id = f.folder_id
    LEFT JOIN file_tag_text ft ON ft.file_id = f.id
    WHERE f.workspace_id = $1
      AND f.deleted_at IS NULL
      AND parent.deleted_at IS NULL
      AND parent.encryption_mode <> 'strict_zk'
      AND immutable_unaccent($2) <%
          immutable_unaccent(f.name || ' ' || COALESCE(ft.tag_text, '') || ' ' || COALESCE(f.content_text, ''))
    ORDER BY rank DESC, f.created_at DESC
    LIMIT $3
),
trgm_folders AS (
    SELECT 'folder'::TEXT AS type,
           fo.id,
           fo.name,
           fo.path AS path,
           NULL::UUID AS folder_id,
           fo.created_at,
           word_similarity(immutable_unaccent($2), immutable_unaccent(fo.name))::float4 AS rank,
           ARRAY[]::TEXT[] AS tags
    FROM folders fo
    WHERE fo.workspace_id = $1
      AND fo.deleted_at IS NULL
      AND fo.encryption_mode <> 'strict_zk'
      AND immutable_unaccent($2) <% immutable_unaccent(fo.name)
    ORDER BY rank DESC, fo.created_at DESC
    LIMIT $3
),
unioned_results AS (
    SELECT * FROM fts_files
    UNION ALL
    SELECT * FROM fts_folders
    UNION ALL
    SELECT * FROM trgm_files
    UNION ALL
    SELECT * FROM trgm_folders
),
ranked AS (
    -- DISTINCT ON keeps the higher-ranked copy when both FTS and
    -- trigram surface the same row. Ordering by type, id, rank
    -- DESC ensures the FIRST row per (type, id) is the best
    -- scoring one.
    SELECT DISTINCT ON (type, id)
           type, id, name, path, folder_id, created_at, rank, tags
    FROM unioned_results
    ORDER BY type, id, rank DESC, created_at DESC
)
SELECT type, id, name, path, folder_id, created_at, rank, tags
FROM ranked
ORDER BY rank DESC, created_at DESC
LIMIT $4 OFFSET $5`

func scanResult(rows pgx.Rows) (Result, error) {
	var (
		r        Result
		folderID *uuid.UUID
		tags     []string
	)
	if err := rows.Scan(&r.Type, &r.ID, &r.Name, &r.Path, &folderID, &r.CreatedAt, &r.Rank, &tags); err != nil {
		return Result{}, err
	}
	if folderID != nil {
		r.FolderID = *folderID
	}
	if len(tags) > 0 {
		r.Tags = tags
	}
	return r, nil
}
