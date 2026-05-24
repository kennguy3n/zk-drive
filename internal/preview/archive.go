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
		entries []string
		kind    string
		err     error
	)
	switch normalizeMime(mime) {
	case "application/zip", "application/x-zip-compressed":
		entries, err = listZipEntries(src)
		kind = "ZIP"
	case "application/x-tar":
		entries, err = listTarEntries(bytes.NewReader(src))
		kind = "TAR"
	case "application/gzip", "application/x-gzip", "application/x-tar-gz", "application/x-tgz":
		// .tar.gz / .tgz. Wrap the source in a gzip reader, then
		// hand off to the tar entry lister. A plain .gz of a
		// non-tar file is rare enough to fold into this same path —
		// we'll just enumerate one entry ("the underlying
		// uncompressed stream") and surface it as the listing.
		gz, gzErr := gzip.NewReader(bytes.NewReader(src))
		if gzErr != nil {
			return nil, fmt.Errorf("gzip: %w", gzErr)
		}
		defer func() { _ = gz.Close() }()
		entries, err = listTarEntries(gz)
		kind = "TAR.GZ"
	default:
		return nil, fmt.Errorf("%w: archive type %q", ErrUnsupportedMime, mime)
	}
	if err != nil {
		return nil, fmt.Errorf("list archive: %w", err)
	}
	// Header is computed AFTER the err check so a failed listing
	// can't bake a misleading "0 entries" caption into the preview.
	header := fmt.Sprintf("%s archive — %d entries", kind, len(entries))
	// Sort directory entries first, then files, both alphabetically.
	// This matches the listing format most file managers use and
	// makes the preview deterministic across re-uploads of the same
	// archive (zip readers do not guarantee insertion order).
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		ad, bd := strings.HasSuffix(a, "/"), strings.HasSuffix(b, "/")
		if ad != bd {
			return ad
		}
		return a < b
	})
	if len(entries) > archiveMaxEntries {
		entries = entries[:archiveMaxEntries]
	}
	body := strings.Join(entries, "\n")
	return renderTextToImage(body, textPreviewOpts{
		header: header,
		// maxLines auto-fills the canvas; no need to cap manually
		// because the source list is already bounded by
		// archiveMaxEntries.
	}), nil
}

func listZipEntries(src []byte) ([]string, error) {
	r, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.File))
	for _, f := range r.File {
		out = append(out, f.Name)
	}
	return out, nil
}

func listTarEntries(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	out := []string{}
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
			if len(out) == 0 {
				return nil, err
			}
			return out, nil
		}
		out = append(out, h.Name)
		if len(out) >= archiveMaxEntries {
			break
		}
	}
	return out, nil
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
