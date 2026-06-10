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

// Preview status values mirror migration 040's CHECK constraint on
// file_versions.preview_status. Exposing them as typed constants keeps
// the string set discoverable from Go and prevents the worker and
// repository from drifting on literal strings.
const (
	StatusPending     = "pending"
	StatusDone        = "done"
	StatusUnsupported = "unsupported"
	StatusFailed      = "failed"
)

// PreviewMaxAttempts is the number of consecutive failed deliveries
// after which the worker marks a preview job preview_failed in the DB
// and acks it (skips) rather than Nak-looping until JetStream's
// QueueMaxDeliver cap. WS8 8.4 specifies three attempts: enough for a
// transient storage/renderer blip to recover, few enough that a
// genuinely poison payload terminates within a couple of AckWait
// cycles instead of producing minutes of redelivery churn.
const PreviewMaxAttempts = 3

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

// MaxDeliver is the upper bound on how many times JetStream will
// redeliver a preview job that the worker keeps Nak'ing. Without
// this cap, a deterministic-failure renderer error (e.g. a truly
// corrupt file that fails to decode on every retry) would loop
// until JetStream's stream-level MaxAge expired, eating worker
// goroutine time and producing repeated error logs.
//
// Five attempts mirrors webhooks.MaxAttempts and gives transient
// failures (storage timeout, brief NATS partition) a few cycles to
// recover before the message is terminated. ErrUnsupportedMime
// already short-circuits to a graceful Ack at attempt 1, so this
// cap only affects the "decode error / IO error" path.
const MaxDeliver = 5
