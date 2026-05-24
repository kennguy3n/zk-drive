package preview

import (
	"context"
	"image"
)

// textPreviewMaxBytes is the upper bound on how much of a text /
// source-code file we read into the preview. The full file is
// downloaded already (capped by MaxSourceBytes), but for the actual
// rendering we only need the first ~16 KiB — that's plenty to fill
// the visible canvas of a 256 px thumbnail at our text density, and
// avoids spending CPU on rune-wrapping multi-megabyte log files.
const textPreviewMaxBytes = 16 * 1024

// textPreviewMaxLines is the cap on visible lines in a text preview.
// Roughly fills the 600 px working canvas at the current font size
// (inconsolata 8x16 → ~16 px line height including leading → ~35
// drawable lines), with a small buffer for headers.
const textPreviewMaxLines = 32

// renderText decodes the source bytes as UTF-8 text and rasterises
// the first textPreviewMaxLines lines onto a fixed-size canvas. The
// canvas is then resized to ThumbnailSize by the service-level
// resize step.
//
// The renderer is intentionally font-and-layout based rather than
// syntax-highlighted: a 256 px PNG cannot carry meaningful colour
// information, the customer-facing value is "what does the start of
// this file look like" plus a recognisable shape, and avoiding a
// chroma / formatter dependency keeps the worker image small and
// fast to boot.
//
// Binary blobs masquerading as text/* won't crash the renderer —
// non-printable runes are mapped to spaces in renderTextToImage.
func renderText(_ context.Context, srcBytes []byte) (image.Image, error) {
	body := srcBytes
	if len(body) > textPreviewMaxBytes {
		body = body[:textPreviewMaxBytes]
	}
	return renderTextToImage(string(body), textPreviewOpts{
		maxLines: textPreviewMaxLines,
	}), nil
}

func init() {
	// MIME types we treat as renderable text. We deliberately cast a
	// wide net: text/* is the obvious group, but a lot of code
	// uploads end up as application/json / application/javascript /
	// application/x-yaml / application/x-sh / etc. by browser MIME
	// guessing, and they're all "monospace and printable" from a
	// preview standpoint.
	mimes := []string{
		"text/plain",
		"text/markdown",
		"text/csv",
		"text/tab-separated-values",
		"text/html",
		"text/css",
		"text/javascript",
		"text/x-go",
		"text/x-rust",
		"text/x-python",
		"text/x-java",
		"text/x-c",
		"text/x-c++",
		"text/x-ruby",
		"text/x-shellscript",
		"application/json",
		"application/x-ndjson",
		"application/javascript",
		"application/typescript",
		"application/xml",
		"text/xml",
		"application/x-yaml",
		"text/yaml",
		"application/x-toml",
		"text/x-toml",
		"application/x-sh",
		"application/x-sql",
		"text/x-sql",
	}
	Register(RendererFunc(renderText), mimes...)
}
