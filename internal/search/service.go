// Package search implements workspace-scoped full-text search across
// files and folders. Phase 2 uses Postgres' built-in `to_tsvector` /
// `plainto_tsquery` operators with the `simple` dictionary so search
// works in every language without requiring a language-specific stemmer
// configuration. Phase 3+ can migrate to pg_trgm or an external engine
// without changing this package's public surface.
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
)

// Result is a single hit in a search response. Type is "file" or
// "folder"; Path is the folder's materialized path (for folders) or the
// parent folder's path (for files) so the frontend can show the hit in
// context without a second round-trip. Rank is the Postgres ts_rank_cd
// score used for ordering, exposed so clients can do client-side
// re-weighting if desired. Tags is non-nil only on file hits; folder
// hits always omit it (folders don't have tags in this phase).
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

// Service wraps a pgxpool.Pool with workspace-scoped FTS queries.
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

// ErrInvalidQuery is returned when the caller supplies an empty query.
var ErrInvalidQuery = errors.New("search: query is required")

// Search runs a Postgres FTS query over files and folders in a single
// workspace. Matches are ranked by ts_rank_cd DESC, with a stable
// secondary sort on created_at DESC so ties are deterministic. Soft-
// deleted rows are excluded (deleted_at IS NULL) and workspace_id is
// bound to the authenticated tenant by the caller.
func (s *Service) Search(ctx context.Context, workspaceID uuid.UUID, query string, limit, offset int) ([]Result, error) {
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

	// The UNION ALL keeps each side cheap (both have a B-tree index on
	// workspace_id plus the soft-delete filter). Ordering happens on the
	// wrapping SELECT so ranks are compared across both resource kinds.
	// File matches LEFT JOIN file_tags so we can both (a) match against
	// tag text in the tsvector and (b) emit the per-file tag list in
	// the response. The aggregated tag list uses array_agg(DISTINCT)
	// inside a subquery so a file with three tags doesn't become three
	// hits.
	const q = `
WITH file_tag_text AS (
    SELECT file_id,
           string_agg(tag, ' ') AS tag_text,
           array_agg(DISTINCT tag ORDER BY tag) AS tags
    FROM file_tags
    WHERE workspace_id = $1
    GROUP BY file_id
),
matches AS (
    SELECT 'file'::TEXT AS type,
           f.id,
           f.name,
           parent.path AS path,
           f.folder_id,
           f.created_at,
           ts_rank_cd(
               to_tsvector('simple', f.name || ' ' || COALESCE(ft.tag_text, '')),
               plainto_tsquery('simple', $2)
           ) AS rank,
           COALESCE(ft.tags, ARRAY[]::TEXT[]) AS tags
    FROM files f
    JOIN folders parent ON parent.id = f.folder_id
    LEFT JOIN file_tag_text ft ON ft.file_id = f.id
    WHERE f.workspace_id = $1
      AND f.deleted_at IS NULL
      AND parent.deleted_at IS NULL
      AND to_tsvector('simple', f.name || ' ' || COALESCE(ft.tag_text, ''))
          @@ plainto_tsquery('simple', $2)
    UNION ALL
    SELECT 'folder'::TEXT AS type,
           fo.id,
           fo.name,
           fo.path AS path,
           NULL::UUID AS folder_id,
           fo.created_at,
           ts_rank_cd(to_tsvector('simple', fo.name), plainto_tsquery('simple', $2)) AS rank,
           ARRAY[]::TEXT[] AS tags
    FROM folders fo
    WHERE fo.workspace_id = $1
      AND fo.deleted_at IS NULL
      AND to_tsvector('simple', fo.name) @@ plainto_tsquery('simple', $2)
)
SELECT type, id, name, path, folder_id, created_at, rank, tags
FROM matches
ORDER BY rank DESC, created_at DESC
LIMIT $3 OFFSET $4`

	rows, err := s.pool.Query(ctx, q, workspaceID, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var out []Result
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

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
