// Package preview generates thumbnail previews for uploaded file
// versions. The initial implementation supports only image types
// (PNG / JPEG / GIF / WebP-via-stdlib) using pure-Go decoders and
// the BSD-3-Clause x/image resampler. PDF / office document support is
// planned for a later sprint and will shell out to ImageMagick
// (Apache-2.0) and LibreOffice headless (MPL-2.0); both are acceptable
// for a proprietary product when used as external tools.
//
// Preview objects are uploaded to the same zk-object-fabric bucket as
// the source file under the key
//   {workspace_id}/{file_id}/{version_id}/preview.png
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
