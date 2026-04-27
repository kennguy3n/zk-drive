// Package classify assigns a coarse "what kind of thing is this"
// label to a file row. Phase 4 keeps the taxonomy rule-based so the
// workflow (migration + worker job + persisted column) is wired end
// to end without a model dependency; a later phase can swap in a
// real classifier behind the same Service.Classify contract.
package classify

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Classification labels assigned by the rule-based scaffold. Kept as
// typed constants so the handful of call-sites that care (tests,
// admin surfaces) don't have to repeat the literal strings.
const (
	LabelImage    = "image"
	LabelInvoice  = "invoice"
	LabelContract = "contract"
	LabelDocument = "document"
	LabelOther    = "other"
)

// Service classifies files by id. It only needs the pool — there is
// no external dependency yet because the logic is a handful of
// string comparisons.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service bound to pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Classify resolves the file by id, decides a label from its name +
// mime type, and persists the label back onto the files row. The
// current rule set is intentionally small; rules are ordered most
// specific → least so "invoice.pdf" picks LabelInvoice rather than
// the generic LabelDocument.
func (s *Service) Classify(ctx context.Context, fileID uuid.UUID) error {
	var name, mime string
	err := s.pool.QueryRow(ctx,
		`SELECT name, COALESCE(mime_type, '') FROM files WHERE id = $1 AND deleted_at IS NULL`,
		fileID).Scan(&name, &mime)
	if err != nil {
		return fmt.Errorf("classify: load file %s: %w", fileID, err)
	}

	label := labelFor(name, mime)
	if _, err := s.pool.Exec(ctx,
		`UPDATE files SET classification = $1 WHERE id = $2`,
		label, fileID); err != nil {
		return fmt.Errorf("classify: update file %s: %w", fileID, err)
	}
	return nil
}

// labelFor is the pure rule table, exported indirectly through
// Classify. Exposed as a package-level function to keep it unit-
// testable without a database.
func labelFor(name, mime string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return LabelImage
	case strings.Contains(lower, "invoice"):
		return LabelInvoice
	case strings.Contains(lower, "contract"):
		return LabelContract
	case mime == "application/pdf":
		return LabelDocument
	default:
		return LabelOther
	}
}
