package preview

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"image"
	"io"
	"sort"
	"strings"
)

// archiveMaxEntries caps the number of entries we'll enumerate from
// an archive. We only need enough to fill the preview canvas; chasing
// every entry of a 50k-file tarball would be a waste of CPU and
// memory.
const archiveMaxEntries = 64

// renderArchive lists the file paths inside a ZIP / TAR / TAR.GZ
// archive and renders them as a text image. We do NOT extract — the
// preview only shows what's inside, never content of inner files.
// This also makes the renderer cheap and safe (no risk of running
// untrusted nested archives through other renderers).
//
// MIME dispatch decides which decoder to use. The handler accepts
// both "application/zip" and "application/x-zip-compressed" because
// browsers and OS sniffers disagree on the canonical type.
func renderArchive(_ context.Context, mime string, src []byte) (image.Image, error) {
	var (
		entries    []string
		totalCount int
		kind       string
		err        error
	)
	switch normalizeMime(mime) {
	case "application/zip", "application/x-zip-compressed":
		entries, totalCount, err = listZipEntries(src)
		kind = "ZIP"
	case "application/x-tar":
		entries, totalCount, err = listTarEntries(bytes.NewReader(src))
		kind = "TAR"
	case "application/gzip", "application/x-gzip", "application/x-tar-gz", "application/x-tgz":
		// .tar.gz / .tgz. Try the tar path first; if the
		// decompressed bytes aren't a valid tar archive (i.e. this
		// is a plain .gz of some other file), fall back to a
		// synthetic single-entry listing showing the embedded
		// filename from the gzip header (or a generic placeholder
		// if it's absent). The fallback is important for worker
		// stability: without it, a plain .gz upload returned a
		// tar-parse error that did NOT wrap ErrUnsupportedMime, so
		// the worker's Nak loop retried the job until the JetStream
		// delivery cap kicked in.
		entries, totalCount, kind, err = listGzipOrTarGzEntries(src)
	default:
		return nil, fmt.Errorf("%w: archive type %q", ErrUnsupportedMime, mime)
	}
	if err != nil {
		return nil, fmt.Errorf("list archive: %w", err)
	}
	// Header is computed AFTER the err check so a failed listing
	// can't bake a misleading "0 entries" caption into the preview.
	// totalCount reflects the real number of entries the listers
	// observed, even when archiveMaxEntries truncated the returned
	// slice. Surface that fact in the caption so a 50k-file tarball
	// doesn't render as "TAR archive — 64 entries".
	header := fmt.Sprintf("%s archive — %d entries", kind, totalCount)
	if totalCount > len(entries) {
		header = fmt.Sprintf("%s archive — %d entries (showing first %d)", kind, totalCount, len(entries))
	}
	// Sort directory entries first, then files, both alphabetically.
	// This matches the listing format most file managers use and
	// makes the preview deterministic across re-uploads of the same
	// archive (zip readers do not guarantee insertion order). The
	// listers already cap entries at archiveMaxEntries so we sort a
	// bounded slice rather than a potentially-millions-of-entries
	// one — see the doc comment on listZipEntries.
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		ad, bd := strings.HasSuffix(a, "/"), strings.HasSuffix(b, "/")
		if ad != bd {
			return ad
		}
		return a < b
	})
	body := strings.Join(entries, "\n")
	return renderTextToImage(body, textPreviewOpts{
		header: header,
		// maxLines auto-fills the canvas; no need to cap manually
		// because the source list is already bounded by
		// archiveMaxEntries.
	}), nil
}

// listGzipOrTarGzEntries decompresses a gzip blob and tries to walk
// it as a tar stream. On success it returns the tar listing tagged
// as TAR.GZ. On failure — i.e. the gzip is valid but its contents
// aren't tar — it falls back to a synthetic single-entry listing
// tagged as GZ that surfaces the original filename from the gzip
// header (RFC 1952 section 2.3.1.5). The synthetic listing keeps
// the worker from looping on Nak/redeliver for plain-gzip uploads
// while still giving the user something useful in the preview.
//
// Returns an error only when the input isn't a valid gzip stream at
// all (e.g. truncated upload, wrong MIME). That error path is rare
// enough that we let it propagate as a hard failure.
func listGzipOrTarGzEntries(src []byte) ([]string, int, string, error) {
	// Two readers because tar walks consume the stream. We probe
	// the tar shape on a fresh reader, then either keep going with
	// the tar listing or rewind to grab the gzip header name for
	// the synthetic listing.
	gz, gzErr := gzip.NewReader(bytes.NewReader(src))
	if gzErr != nil {
		return nil, 0, "", fmt.Errorf("gzip: %w", gzErr)
	}
	defer func() { _ = gz.Close() }()
	entries, total, tarErr := listTarEntries(gz)
	if tarErr == nil && total > 0 {
		return entries, total, "TAR.GZ", nil
	}
	// Either tar parsing failed or the stream was zero-entry tar
	// (which is, in practice, never — anything that reads as
	// "valid tar with 0 entries" is almost always a plain gzip
	// blob with content that happens to align). Either way, fall
	// back to the synthetic listing.
	gz2, gz2Err := gzip.NewReader(bytes.NewReader(src))
	if gz2Err != nil {
		return nil, 0, "", fmt.Errorf("gzip: %w", gz2Err)
	}
	defer func() { _ = gz2.Close() }()
	name := gz2.Name
	if name == "" {
		name = "(compressed content)"
	}
	return []string{name}, 1, "GZ", nil
}

// listZipEntries reads the ZIP central directory and returns the
// first archiveMaxEntries entry names plus the TOTAL count of entries
// the archive contained. The cap is enforced during the iteration,
// not afterwards: a crafted ZIP can declare hundreds of thousands of
// tiny entries within our 100 MiB MaxSourceBytes budget, and the
// previous "collect everything, sort it, then slice" implementation
// would let one such job allocate megabytes of string headers and
// hammer the worker with O(n log n) sorting before any output was
// produced. Capping early means the work per preview is bounded by
// archiveMaxEntries regardless of how pathological the input is.
func listZipEntries(src []byte) ([]string, int, error) {
	r, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, 0, err
	}
	preallocCount := len(r.File)
	if preallocCount > archiveMaxEntries {
		preallocCount = archiveMaxEntries
	}
	out := make([]string, 0, preallocCount)
	for _, f := range r.File {
		if len(out) >= archiveMaxEntries {
			break
		}
		out = append(out, f.Name)
	}
	return out, len(r.File), nil
}

// listTarEntries walks the tar stream and returns the first
// archiveMaxEntries entry names plus the TOTAL number of entries the
// stream contained. The total is counted incrementally (we can't ask
// a tar stream for its length up front) so we keep iterating past
// the cap to drain the headers — but we DON'T append past the cap,
// so the per-preview memory is still bounded by archiveMaxEntries.
func listTarEntries(r io.Reader) ([]string, int, error) {
	tr := tar.NewReader(r)
	out := []string{}
	total := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Non-fatal: many .tar.gz with corrupt trailing bytes
			// still have a valid prefix of entries. Return what we
			// have so the preview still renders, but wrap the error
			// in case callers care.
			if total == 0 {
				return nil, 0, err
			}
			return out, total, nil
		}
		total++
		if len(out) < archiveMaxEntries {
			out = append(out, h.Name)
		}
	}
	return out, total, nil
}

func init() {
	mimes := []string{
		"application/zip",
		"application/x-zip-compressed",
		"application/x-tar",
		"application/gzip",
		"application/x-gzip",
		"application/x-tar-gz",
		"application/x-tgz",
	}
	for _, m := range mimes {
		mime := m
		Register(RendererFunc(func(ctx context.Context, src []byte) (image.Image, error) {
			return renderArchive(ctx, mime, src)
		}), mime)
	}
}
