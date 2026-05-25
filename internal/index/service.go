// Package index extracts text from uploaded files and writes it to
// files.content_text so the Postgres FTS query can score on body
// content in addition to file name and tag list.
//
// The worker calls IndexFile after a successful upload (drive.search.index
// subject). Tests bypass the storage round-trip by calling
// PersistContent directly with a known plaintext body.
package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// MaxIndexBytes caps the final extracted text written to
// files.content_text. Past this point the FTS gain is marginal and
// the Postgres column begins to dominate per-row size. Applied as a
// rune-boundary truncate to the extractor output, NOT as a cap on the
// downloaded blob — PDF / DOCX are binary formats where truncating
// the source bytes produces a corrupt input that the extractor would
// then fail on (a zip central directory lives at the end of the
// archive; a PDF xref table at the end of the file).
const MaxIndexBytes int64 = 4 << 20 // 4 MiB

// MaxDownloadBytes caps how much of an uploaded object the worker
// will pull down before extraction. Mirrors MaxSourceBytes /
// MaxScanBytes used by the preview and scan workers — same order
// of magnitude (100 MiB) so a single worker job is bounded by a
// predictable amount of network and memory regardless of which
// pipeline it lands in. Files larger than this surface a hard
// "exceeds N bytes" error and the worker NAKs the job rather than
// silently writing a partial index that would mis-rank search hits.
const MaxDownloadBytes int64 = 100 << 20 // 100 MiB

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
	text, err := ExtractTextWithContext(ctx, row.mimeType, body)
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
// types and the structured-text application/json|xml subtypes are
// passed through verbatim (truncated to MaxIndexBytes characters on
// a UTF-8 rune boundary).
//
// application/pdf shells out to pdftotext (poppler-utils) — see
// extractPDFText for the graceful-skip semantics when the binary is
// missing.
//
// application/vnd.openxmlformats-officedocument.wordprocessingml.document
// (.docx) is parsed in-process via archive/zip + encoding/xml —
// see extractDOCXText.
//
// All other mime types return ErrUnsupportedMimeType so the worker
// acks the message without writing partial content — the file is
// still searchable by name + tags.
func ExtractText(mimeType string, body []byte) (string, error) {
	return ExtractTextWithContext(context.Background(), mimeType, body)
}

// ExtractTextWithContext is the context-aware form of ExtractText.
// Used by the worker path (IndexFile) so a slow pdftotext invocation
// can be cancelled when the job context expires. Existing callers
// that don't have a context can continue to use ExtractText, which
// passes context.Background().
func ExtractTextWithContext(ctx context.Context, mimeType string, body []byte) (string, error) {
	mt := normalizeMimeType(mimeType)
	if mt == "" {
		return "", ErrUnsupportedMimeType
	}
	switch {
	// Specific text/* MIME types must come BEFORE the generic
	// `text/` prefix branch below. Otherwise the prefix match
	// shadows extractor-specific handlers and the worker writes
	// raw markup (HTML tags / RTF control codes) straight into
	// content_text — polluting the FTS index and breaking phrase
	// queries.
	case mt == "text/html":
		text, err := extractHTMLText(body)
		if err != nil {
			return "", err
		}
		return truncateUTF8(text, MaxIndexBytes), nil
	case mt == "application/rtf", mt == "text/rtf":
		text, err := extractRTFText(body)
		if err != nil {
			return "", err
		}
		return truncateUTF8(text, MaxIndexBytes), nil
	case strings.HasPrefix(mt, "text/"):
		return truncateUTF8(string(body), MaxIndexBytes), nil
	case mt == "application/json", mt == "application/xml":
		return truncateUTF8(string(body), MaxIndexBytes), nil
	case mt == "application/pdf":
		text, err := extractPDFText(ctx, body)
		if err != nil {
			return "", err
		}
		return truncateUTF8(text, MaxIndexBytes), nil
	case mt == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		text, err := extractDOCXText(body)
		if err != nil {
			return "", err
		}
		return truncateUTF8(text, MaxIndexBytes), nil
	case mt == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		text, err := extractXLSXText(body)
		if err != nil {
			return "", err
		}
		return truncateUTF8(text, MaxIndexBytes), nil
	case mt == "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		text, err := extractPPTXText(body)
		if err != nil {
			return "", err
		}
		return truncateUTF8(text, MaxIndexBytes), nil
	case mt == "application/vnd.oasis.opendocument.text",
		mt == "application/vnd.oasis.opendocument.spreadsheet",
		mt == "application/vnd.oasis.opendocument.presentation":
		text, err := extractOpenDocumentText(body)
		if err != nil {
			return "", err
		}
		return truncateUTF8(text, MaxIndexBytes), nil
	default:
		return "", ErrUnsupportedMimeType
	}
}

// normalizeMimeType strips RFC 2045 parameters (e.g.
// "application/pdf; charset=utf-8" → "application/pdf"), lowercases
// the result, and trims surrounding whitespace. This keeps the
// switch-case dispatch above robust regardless of whether the upload
// API ever starts persisting parameters on the files.mime_type
// column. Today the frontend sends bare types (api/drive/upload.go),
// so the parametrised case never fires in production — but parsing
// here means a future change that does forward parameters will keep
// hitting the correct extractor branch instead of falling through to
// ErrUnsupportedMimeType.
//
// If the input is malformed and mime.ParseMediaType fails, we fall
// back to the previous behaviour (lower + trim) so a legacy value
// like "APPLICATION/PDF " (no parameters but bad casing) still
// routes correctly.
func normalizeMimeType(mimeType string) string {
	trimmed := strings.TrimSpace(mimeType)
	if trimmed == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(trimmed); err == nil {
		return strings.ToLower(mt)
	}
	return strings.ToLower(trimmed)
}

// truncateUTF8 trims s so it ends on a rune boundary and its byte
// length does not exceed max. Postgres rejects invalid UTF-8 on the
// content_text column, so a raw byte-offset slice can cause the
// worker to loop on redelivery when a multi-byte rune straddles the
// cap. Also strips any trailing invalid byte sequences from the
// already-valid prefix.
func truncateUTF8(s string, max int64) string {
	if int64(len(s)) > max {
		s = s[:max]
	}
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("index: download: %d %s", resp.StatusCode, string(body))
	}
	// Read MaxDownloadBytes+1 so we can distinguish a file that is
	// exactly MaxDownloadBytes long from one that overflows the cap.
	// Silent truncation is not acceptable: PDF / DOCX both store their
	// directory structures at the end of the file, so a truncated
	// download would produce extraction errors that mask the real
	// problem (the object is too large to index).
	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("index: read body: %w", err)
	}
	if int64(len(buf)) > MaxDownloadBytes {
		return nil, fmt.Errorf("index: object exceeds %d bytes", MaxDownloadBytes)
	}
	return buf, nil
}
