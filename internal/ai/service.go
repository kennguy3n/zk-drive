// Package ai hosts thread/room summary services for ZK Drive. The
// current implementation is a rule-based scaffold: it assembles a
// fixed-format summary string from folder file names and any
// indexed content_text. A future change will plug in an actual LLM
// behind the same Summarize contract; until then the scaffold is
// enough to exercise the end-to-end privacy guarantee (strict-ZK
// folders refuse to produce a summary at all).
package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// ErrStrictZKForbidden is returned when Summarize is asked to produce
// a summary for a strict-zero-knowledge folder. The server never has
// access to plaintext for those folders, so honestly there is nothing
// we could summarise; we surface the refusal explicitly so callers
// (handlers) can map it to 403 rather than returning an empty string.
var ErrStrictZKForbidden = errors.New("ai: summary not available for strict-zk folders")

// ErrFolderNotFound is returned when the folder row cannot be
// located. Handlers map this to 404 to match the regular drive API.
var ErrFolderNotFound = errors.New("ai: folder not found")

// maxFilesPerSummary caps the number of files the scaffold considers.
// The cap keeps the generated string bounded; real LLM wiring will
// revisit this alongside token-budgeting.
const maxFilesPerSummary = 50

// previewBytesPerFile caps how many characters of content_text each
// file contributes to the preview buffer.
const previewBytesPerFile = 500

// previewBytesTotal caps the number of characters from the joined
// file previews that make it into the summary string.
const previewBytesTotal = 200

// SummaryService produces a textual summary of a folder's contents.
// A single pgxpool is enough — summaries read files + folders but
// don't hit external services in the rule-based mode.
type SummaryService struct {
	pool *pgxpool.Pool
}

// NewSummaryService returns a SummaryService bound to pool. A nil pool
// is treated as a misconfiguration and panics on first use rather
// than silently returning empty summaries.
func NewSummaryService(pool *pgxpool.Pool) *SummaryService {
	return &SummaryService{pool: pool}
}

// Summarize produces a placeholder summary of the files currently in
// folderID. Strict-ZK folders short-circuit with ErrStrictZKForbidden
// — the server has no plaintext for them by design. Managed folders
// return a deterministic string of the form "Room contains N files:
// [f1, f2, …]. Content preview: [first 200 chars]" so integration
// tests can assert a stable shape.
func (s *SummaryService) Summarize(ctx context.Context, workspaceID, folderID uuid.UUID) (string, error) {
	if s.pool == nil {
		return "", errors.New("ai: summary service not configured")
	}

	var mode string
	err := s.pool.QueryRow(ctx,
		`SELECT encryption_mode FROM folders WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL`,
		folderID, workspaceID).Scan(&mode)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrFolderNotFound, err)
	}
	if mode == folder.EncryptionStrictZK {
		return "", ErrStrictZKForbidden
	}

	rows, err := s.pool.Query(ctx,
		`SELECT name, COALESCE(content_text, '')
		 FROM files
		 WHERE folder_id = $1 AND workspace_id = $2 AND deleted_at IS NULL
		 ORDER BY created_at ASC
		 LIMIT $3`,
		folderID, workspaceID, maxFilesPerSummary)
	if err != nil {
		return "", fmt.Errorf("ai: query files: %w", err)
	}
	defer rows.Close()

	names := make([]string, 0, maxFilesPerSummary)
	var previewBuf strings.Builder
	for rows.Next() {
		var name, content string
		if err := rows.Scan(&name, &content); err != nil {
			return "", fmt.Errorf("ai: scan files: %w", err)
		}
		names = append(names, name)
		if content != "" {
			snippet := content
			if len(snippet) > previewBytesPerFile {
				snippet = snippet[:previewBytesPerFile]
			}
			previewBuf.WriteString(snippet)
			previewBuf.WriteString(" ")
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("ai: iterate files: %w", err)
	}

	preview := strings.TrimSpace(previewBuf.String())
	if len(preview) > previewBytesTotal {
		preview = preview[:previewBytesTotal]
	}

	return fmt.Sprintf("Room contains %d files: [%s]. Content preview: [%s]",
		len(names), strings.Join(names, ", "), preview), nil
}
