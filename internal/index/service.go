// Package index extracts text from uploaded files and writes it to
// files.content_text so the Postgres FTS query can score on body
// content in addition to file name and tag list.
//
// The worker calls IndexFile after a successful upload (drive.search.index
// subject). Tests bypass the storage round-trip by calling
// PersistContent directly with a known plaintext body.
package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// MaxIndexBytes caps how much body content the worker will read from
// a single object. Past this point the FTS gain is marginal and the
// memory pressure becomes a problem on shared workers.
const MaxIndexBytes int64 = 4 << 20 // 4 MiB

// ErrUnsupportedMimeType is returned by ExtractText when the worker
// has no text extractor for the supplied content type. Callers ack
// the message without writing content_text — the file is still
// searchable by name + tags.
var ErrUnsupportedMimeType = errors.New("index: unsupported mime type for text extraction")

// Service downloads uploaded objects, extracts plain text, and
// persists it to files.content_text. The HTTP client is injectable so
// tests can wire a fake transport.
type Service struct {
	pool    *pgxpool.Pool
	storage *storage.Client
	client  *http.Client
}

// NewService builds an index Service. A nil http.Client uses the
// default client with a 60s timeout, matching the archive worker.
func NewService(pool *pgxpool.Pool, st *storage.Client, httpClient *http.Client) *Service {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Service{pool: pool, storage: st, client: httpClient}
}

// IndexFile resolves the file's current object key, downloads the
// bytes via presigned GET, extracts text, and writes the result to
// files.content_text. Strict-ZK files must be filtered out by the
// caller (worker checks this via folder.EncryptionModeForFile before
// calling).
func (s *Service) IndexFile(ctx context.Context, fileID, versionID uuid.UUID) error {
	if s.storage == nil {
		return errors.New("index: storage client not configured")
	}
	row, err := s.loadVersion(ctx, fileID, versionID)
	if err != nil {
		return err
	}
	getURL, err := s.storage.GenerateDownloadURL(ctx, row.objectKey, storage.DefaultPresignExpiry)
	if err != nil {
		return fmt.Errorf("index: sign get: %w", err)
	}
	body, err := s.fetch(ctx, getURL)
	if err != nil {
		return err
	}
	text, err := ExtractText(row.mimeType, body)
	if err != nil {
		if errors.Is(err, ErrUnsupportedMimeType) {
			return nil
		}
		return err
	}
	return s.PersistContent(ctx, fileID, text)
}

// PersistContent writes text into files.content_text. Exposed so
// tests can drive the FTS path without spinning up a storage gateway.
// The function is safe to call repeatedly — re-indexing simply
// overwrites the column.
func (s *Service) PersistContent(ctx context.Context, fileID uuid.UUID, text string) error {
	_, err := s.pool.Exec(ctx, `UPDATE files SET content_text = $2 WHERE id = $1`, fileID, text)
	if err != nil {
		return fmt.Errorf("index: persist content_text: %w", err)
	}
	return nil
}

// ExtractText returns plain text for an object body. text/* mime
// types are passed through verbatim (truncated to MaxIndexBytes
// characters); application/pdf returns ErrUnsupportedMimeType for
// now — a future change will shell out to pdftotext or wire a
// pure-Go extractor.
func ExtractText(mimeType string, body []byte) (string, error) {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	if mt == "" {
		return "", ErrUnsupportedMimeType
	}
	switch {
	case strings.HasPrefix(mt, "text/"):
		s := string(body)
		if int64(len(s)) > MaxIndexBytes {
			s = s[:MaxIndexBytes]
		}
		return s, nil
	case mt == "application/json", mt == "application/xml":
		return string(body), nil
	default:
		return "", ErrUnsupportedMimeType
	}
}

type versionRow struct {
	objectKey string
	mimeType  string
}

func (s *Service) loadVersion(ctx context.Context, fileID, versionID uuid.UUID) (*versionRow, error) {
	const q = `
SELECT v.object_key, f.mime_type
FROM file_versions v
JOIN files f ON f.id = v.file_id
WHERE v.id = $2 AND v.file_id = $1`
	row := &versionRow{}
	if err := s.pool.QueryRow(ctx, q, fileID, versionID).Scan(&row.objectKey, &row.mimeType); err != nil {
		return nil, fmt.Errorf("index: load version: %w", err)
	}
	return row, nil
}

func (s *Service) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("index: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("index: download: %d %s", resp.StatusCode, string(body))
	}
	limited := io.LimitReader(resp.Body, MaxIndexBytes+1)
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, limited); err != nil {
		return nil, fmt.Errorf("index: read body: %w", err)
	}
	return buf.Bytes(), nil
}
