// Package preview generates thumbnail previews for uploaded file
// versions.
//
// The package is organised around a MIME-type → Renderer registry
// (see renderer.go). Each format family lives in its own file with
// an init() that registers its renderer for the MIME types it
// supports — adding a new format never touches the service. The
// service downloads the source, looks up the renderer, hands it the
// bytes, then resizes and PNG-encodes whatever in-memory image the
// renderer returned.
//
// Renderers fall into two categories:
//
//   - Pure-Go: image (PNG / JPEG / GIF / WebP), text & code, archive
//     listings, and email (RFC 5322).
//   - Subprocess: pdf (pdftoppm, GPL), office (LibreOffice
//     MPL-2.0 → pdftoppm), video (ffmpeg, LGPL), audio (audiowaveform
//     BBC or ffmpeg fallback), svg (rsvg-convert LGPL), design / PSD
//     / AI / TIFF / HEIC (ImageMagick).
//
// All external binaries are shelled out, never linked, so they do
// not affect the proprietary build's licence. Each handler returns
// ErrUnsupportedMime (via missingBinaryErr) when its binary is
// missing so a misconfigured host yields a graceful "skip this job"
// rather than an infinite Nak loop.
//
// Preview objects are uploaded to the same zk-object-fabric bucket as
// the source file under the key
//
//	{workspace_id}/{file_id}/{version_id}/preview.png
//
// and indexed in the file_previews table so the API layer can resolve
// a preview URL without scanning the bucket.
package preview

import (
	"time"

	"github.com/google/uuid"
)

// Preview is the metadata row for a server-rendered thumbnail.
type Preview struct {
	ID        uuid.UUID `json:"id"`
	FileID    uuid.UUID `json:"file_id"`
	VersionID uuid.UUID `json:"version_id"`
	ObjectKey string    `json:"object_key"`
	MimeType  string    `json:"mime_type"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// ThumbnailSize is the target bounding box (in pixels) for every
// generated preview. 256 px keeps the thumbnails small enough for fast
// grid rendering on the frontend and cheap to store.
const ThumbnailSize = 256

// PreviewMimeType is the output mime type for every preview object.
// Pinning to PNG sidesteps JPEG quality knobs and keeps cache keys
// trivial (extension always .png).
const PreviewMimeType = "image/png"

// MaxSourceBytes caps the source bytes the preview worker is willing
// to read for a single job. 100 MiB comfortably fits every image type
// we render today and prevents a pathologically large upload (e.g. a
// multi-gigabyte PNG) from OOM'ing the worker. Matches the defensive
// cap used by the scan service.
const MaxSourceBytes = 100 * 1024 * 1024
