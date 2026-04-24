package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder
	_ "image/jpeg" // register JPEG decoder
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	xdraw "golang.org/x/image/draw"
)

// ErrUnsupportedMime is returned when the source version is of a type
// the current preview backend does not know how to render.
// Callers (worker) catch this and simply skip the job without marking
// it failed.
var ErrUnsupportedMime = errors.New("preview: unsupported mime type")

// PresignClient is the minimal surface PreviewService needs from the
// storage package. Kept as an interface so tests can stub out S3.
type PresignClient interface {
	GenerateUploadURL(ctx context.Context, objectKey, contentType string, expiry time.Duration) (string, error)
	GenerateDownloadURL(ctx context.Context, objectKey string, expiry time.Duration) (string, error)
}

// Service builds a preview (thumbnail) for a file version:
//  1. look up the version's object_key in Postgres
//  2. download the source bytes via a presigned GET URL
//  3. resize to ThumbnailSize using a bilinear resampler
//  4. upload the preview via a presigned PUT URL
//  5. upsert a file_previews row so the API layer can serve a preview URL
//
// The service is intentionally small; all heavy lifting (decode /
// resize / encode) happens in-process with stdlib + x/image, so CI
// runs on a vanilla Go toolchain.
type Service struct {
	pool    *pgxpool.Pool
	storage PresignClient
	repo    Repository
	httpc   *http.Client
	now     func() time.Time
}

// NewService wires a preview service against the given pool and
// presign client. The http client is a private field so callers that
// need a custom timeout can wrap via SetHTTPClient.
func NewService(pool *pgxpool.Pool, storage PresignClient, repo Repository) *Service {
	return &Service{
		pool:    pool,
		storage: storage,
		repo:    repo,
		httpc:   &http.Client{Timeout: 60 * time.Second},
		now:     time.Now,
	}
}

// SetHTTPClient overrides the HTTP client used for download / upload.
// Intended for tests; production code uses the default 60s-timeout
// client wired by NewService.
func (s *Service) SetHTTPClient(c *http.Client) { s.httpc = c }

// IsSupportedMime reports whether the preview service can render a
// given mime type today. This powers the "Has preview?" hint the
// frontend shows next to files in strict-ZK mode or for types we
// haven't wired yet.
func IsSupportedMime(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch m {
	case "image/png", "image/jpeg", "image/jpg", "image/gif":
		return true
	}
	return false
}

// Generate renders a preview for (fileID, versionID) and persists the
// result. Returns ErrUnsupportedMime when the source is not an image
// the current backend knows how to render.
func (s *Service) Generate(ctx context.Context, fileID, versionID uuid.UUID) (*Preview, error) {
	meta, err := s.fetchVersionMeta(ctx, fileID, versionID)
	if err != nil {
		return nil, err
	}
	if !IsSupportedMime(meta.mimeType) {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedMime, meta.mimeType)
	}

	srcBytes, err := s.downloadObject(ctx, meta.objectKey)
	if err != nil {
		return nil, fmt.Errorf("download source: %w", err)
	}

	img, _, err := image.Decode(bytes.NewReader(srcBytes))
	if err != nil {
		return nil, fmt.Errorf("decode source: %w", err)
	}

	thumb := resize(img, ThumbnailSize, ThumbnailSize)

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, thumb); err != nil {
		return nil, fmt.Errorf("encode preview: %w", err)
	}

	previewKey := PreviewObjectKey(meta.workspaceID, fileID, versionID)
	if err := s.uploadObject(ctx, previewKey, encoded.Bytes()); err != nil {
		return nil, fmt.Errorf("upload preview: %w", err)
	}

	p := &Preview{
		FileID:    fileID,
		VersionID: versionID,
		ObjectKey: previewKey,
		MimeType:  PreviewMimeType,
		SizeBytes: int64(encoded.Len()),
	}
	if err := s.repo.Upsert(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// PreviewObjectKey returns the S3 key used to store a preview. Kept
// as a package-level function so other packages (e.g. handlers that
// need to mint a download URL without going through the service) can
// reconstruct the key from ids.
func PreviewObjectKey(workspaceID, fileID, versionID uuid.UUID) string {
	return fmt.Sprintf("%s/%s/%s/preview.png", workspaceID.String(), fileID.String(), versionID.String())
}

// versionMeta bundles the Postgres columns the service needs to
// render a preview: the workspace it lives in, the source object key,
// and the mime type.
type versionMeta struct {
	workspaceID uuid.UUID
	objectKey   string
	mimeType    string
}

func (s *Service) fetchVersionMeta(ctx context.Context, fileID, versionID uuid.UUID) (versionMeta, error) {
	var m versionMeta
	const q = `
SELECT f.workspace_id, fv.object_key, f.mime_type
FROM file_versions fv
JOIN files f ON f.id = fv.file_id
WHERE fv.id = $1 AND fv.file_id = $2`
	if err := s.pool.QueryRow(ctx, q, versionID, fileID).Scan(&m.workspaceID, &m.objectKey, &m.mimeType); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return m, fmt.Errorf("%w: version %s", ErrNotFound, versionID)
		}
		return m, fmt.Errorf("load version meta: %w", err)
	}
	return m, nil
}

func (s *Service) downloadObject(ctx context.Context, key string) ([]byte, error) {
	url, err := s.storage.GenerateDownloadURL(ctx, key, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("presign get: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("get %s: status %d", key, resp.StatusCode)
	}
	// Cap the source read so a pathologically large image can't OOM
	// the worker. +1 byte on the limit lets us detect overflow
	// distinctly from a file that is exactly MaxSourceBytes long.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > MaxSourceBytes {
		return nil, fmt.Errorf("source %s exceeds %d bytes", key, MaxSourceBytes)
	}
	return buf, nil
}

func (s *Service) uploadObject(ctx context.Context, key string, body []byte) error {
	url, err := s.storage.GenerateUploadURL(ctx, key, PreviewMimeType, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("presign put: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", PreviewMimeType)
	resp, err := s.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("put %s: status %d", key, resp.StatusCode)
	}
	return nil
}

// resize scales img to fit inside (maxW x maxH) while preserving
// aspect ratio. Uses x/image/draw.ApproxBiLinear for speed; visual
// quality is adequate for 256 px thumbnails.
func resize(src image.Image, maxW, maxH int) image.Image {
	b := src.Bounds()
	srcW := b.Dx()
	srcH := b.Dy()
	if srcW == 0 || srcH == 0 {
		// Degenerate input; return a single-pixel preview so we
		// always produce a valid PNG.
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	ratioW := float64(maxW) / float64(srcW)
	ratioH := float64(maxH) / float64(srcH)
	ratio := ratioW
	if ratioH < ratio {
		ratio = ratioH
	}
	if ratio >= 1 {
		// Do not upscale — the preview is just the original.
		dst := image.NewRGBA(b)
		xdraw.Draw(dst, b, src, b.Min, xdraw.Src)
		return dst
	}
	dstW := int(float64(srcW) * ratio)
	dstH := int(float64(srcH) * ratio)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}
