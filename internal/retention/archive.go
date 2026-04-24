package retention

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// ArchiveService compresses expired file versions into a cold archive
// key pattern and stamps archived_at on file_versions. The hot object
// is preserved by default — operators can run a separate prune pass
// once they are satisfied the archive is intact.
type ArchiveService struct {
	pool    *pgxpool.Pool
	storage *storage.Client
	client  *http.Client
}

// NewArchiveService constructs an ArchiveService. The http.Client is
// provided so tests can inject a fake transport; nil uses the default
// client with a conservative 60s timeout.
func NewArchiveService(pool *pgxpool.Pool, st *storage.Client, httpClient *http.Client) *ArchiveService {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &ArchiveService{pool: pool, storage: st, client: httpClient}
}

// ArchiveVersion downloads a single version via presigned GET,
// gzips the bytes in memory, uploads to the archive key pattern via
// presigned PUT, and stamps archived_at on the file_versions row.
//
// The in-memory buffer is acceptable for Phase 3 because the typical
// file size target is <100 MB. Streaming gzip directly between HTTP
// round-trips is a future optimization if this becomes a bottleneck.
func (a *ArchiveService) ArchiveVersion(ctx context.Context, versionID uuid.UUID) error {
	if a.storage == nil {
		return errors.New("archive: storage client not configured")
	}
	row, err := a.loadVersion(ctx, versionID)
	if err != nil {
		return err
	}
	if row.archivedAt != nil {
		return nil
	}

	getURL, err := a.storage.GenerateDownloadURL(ctx, row.objectKey, storage.DefaultPresignExpiry)
	if err != nil {
		return fmt.Errorf("archive: sign get: %w", err)
	}
	compressed, err := a.fetchAndCompress(ctx, getURL)
	if err != nil {
		return err
	}
	archiveKey := fmt.Sprintf("%s/archive/%s/%s.gz", row.workspaceID, row.fileID, row.id)
	putURL, err := a.storage.GenerateUploadURL(ctx, archiveKey, "application/gzip", storage.DefaultPresignExpiry)
	if err != nil {
		return fmt.Errorf("archive: sign put: %w", err)
	}
	if err := a.uploadArchive(ctx, putURL, compressed); err != nil {
		return err
	}
	_, err = a.pool.Exec(ctx,
		`UPDATE file_versions SET archived_at = $2 WHERE id = $1`,
		versionID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("archive: mark archived: %w", err)
	}
	return nil
}

// ArchiveVersions iterates versionIDs and archives each one. Errors
// are collected and the slice of failed ids is returned alongside the
// first wrapped error so the caller can retry the failures later. A
// single failure does not abort the batch — archiving is idempotent.
func (a *ArchiveService) ArchiveVersions(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	var failed []uuid.UUID
	var firstErr error
	for _, id := range ids {
		if ctx.Err() != nil {
			return append(failed, ids[len(failed):]...), ctx.Err()
		}
		if err := a.ArchiveVersion(ctx, id); err != nil {
			failed = append(failed, id)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return failed, firstErr
}

type versionRow struct {
	id          uuid.UUID
	fileID      uuid.UUID
	workspaceID uuid.UUID
	objectKey   string
	archivedAt  *time.Time
}

func (a *ArchiveService) loadVersion(ctx context.Context, id uuid.UUID) (*versionRow, error) {
	const q = `
SELECT v.id, v.file_id, f.workspace_id, v.object_key, v.archived_at
FROM file_versions v
JOIN files f ON f.id = v.file_id
WHERE v.id = $1`
	row := &versionRow{}
	if err := a.pool.QueryRow(ctx, q, id).Scan(&row.id, &row.fileID, &row.workspaceID, &row.objectKey, &row.archivedAt); err != nil {
		return nil, fmt.Errorf("archive: load version: %w", err)
	}
	return row, nil
}

func (a *ArchiveService) fetchAndCompress(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("archive: get hot object: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("archive: get hot object: %d %s", resp.StatusCode, string(body))
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := io.Copy(gz, resp.Body); err != nil {
		return nil, fmt.Errorf("archive: compress: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("archive: close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

func (a *ArchiveService) uploadArchive(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.ContentLength = int64(len(body))
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("archive: put archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("archive: put archive: %d %s", resp.StatusCode, string(b))
	}
	return nil
}
