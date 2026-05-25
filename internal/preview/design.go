package preview

import (
	"context"
	"image"
	"time"
)

// designRenderTimeout caps a single ImageMagick invocation. We rely
// on this rather than the worker-level 2-minute job timeout because
// ImageMagick can spend a surprisingly long time on a single complex
// PSD / AI / HEIC — a multi-hundred-megapixel canvas with a stack
// of adjustment layers can chew CPU for minutes on its own. Without
// this cap, one pathological input would tie up a worker goroutine
// for the full job-timeout window and starve every other preview
// in the queue. 30s matches the office renderer's budget and is
// generous enough for legitimate documents while still being
// tight enough to keep the queue moving.
const designRenderTimeout = 30 * time.Second

// imagemagickBinary is the ImageMagick `convert` command used to
// rasterise design / CAD-adjacent formats. ImageMagick 7 ships
// `magick`; older versions ship `convert`. We default to `convert`
// for maximum compatibility with Debian / Alpine packages and let
// operators override it via SetImageMagickBinary at boot if their
// image only has the newer name.
//
// Wrapped in binaryVar (atomic.Value) so Set + concurrent renderer
// goroutine reads are race-free. See binaryvar.go for the rationale.
var imagemagickBinary = newBinaryVar("convert")

// SetImageMagickBinary lets ops or tests override the ImageMagick
// binary lookup. Exposed (vs. just being a package var) so deploys
// that bake ImageMagick 7 only — which ships `magick` and not
// `convert` — can flip it from a single configuration point at
// worker startup. Safe to call concurrently with renderer reads.
func SetImageMagickBinary(name string) { imagemagickBinary.Set(name) }

// designInputFilenameForMime maps each registered design MIME to the
// filename we hand ImageMagick on disk. The extension matters: even
// though ImageMagick will sniff magic bytes for most formats, some
// edge cases — notably Adobe Illustrator files that are pure
// PDF-wrapped (no `%!PS-` PostScript header) and HEIC variants whose
// initial bytes are ambiguous — only get the correct coder dispatch
// when ImageMagick sees the right extension. Naming every input
// `in.bin` worked for the easy cases (PSD `8BPS`, TIFF `II*` / `MM*`,
// BMP `BM`) but produced "no decode delegate" failures on the
// PDF-only AI and on some HEIF/HEIC files. The map below documents
// the mapping explicitly so future MIME additions remember to wire
// an extension in.
var designInputFilenameForMime = map[string]string{
	// Photoshop — strong magic, but the extension also short-circuits
	// ImageMagick's "try every coder" fallback path.
	"image/vnd.adobe.photoshop":      "in.psd",
	"application/photoshop":          "in.psd",
	"application/x-photoshop":        "in.psd",
	"application/photoshop-document": "in.psd",
	// Illustrator — the load-bearing case for this map. AI files can
	// be wrapped PostScript or wrapped PDF; the PDF-wrapped variant
	// has no `%!PS-` header and ImageMagick needs the `.ai` hint to
	// pick the right coder.
	"application/illustrator": "in.ai",
	// PostScript / EPS — the `%!PS-` magic is reliable but giving the
	// extension is harmless and matches the rest of the table.
	"application/postscript": "in.ps",
	"application/eps":        "in.eps",
	"image/x-eps":            "in.eps",
	// TIFF — `II*\0` / `MM\0*` magic is strong; extension is purely
	// for symmetry with the rest of the table.
	"image/tiff":   "in.tiff",
	"image/x-tiff": "in.tiff",
	// BMP / ICO — `BM` magic is strong but icons need the extension
	// for ImageMagick to pick the multi-image icon coder.
	"image/bmp":                "in.bmp",
	"image/x-bmp":              "in.bmp",
	"image/vnd.microsoft.icon": "in.ico",
	"image/x-icon":             "in.ico",
	// HEIC / HEIF — magic is `ftypheic` / `ftypheif` / `ftypmif1` at
	// offset 4 which isn't always sniffed correctly; the extension
	// pins the heif delegate.
	"image/heic": "in.heic",
	"image/heif": "in.heif",
}

// renderDesign rasterises a design format (PSD, AI, EPS, TIFF) by
// shelling out to ImageMagick.
//
// We use Imagemagick's "first frame" semantics — `in[0]` — so a
// multi-layer PSD or a multi-page AI produces a single thumbnail
// instead of an animated montage. -flatten composites visible
// layers onto a single canvas; -strip drops metadata so the output
// PNG is small.
//
// The input filename's extension is selected from
// designInputFilenameForMime so ImageMagick has both magic bytes
// AND extension to dispatch on — see the comment on the map for
// why this matters.
//
// Returns ErrUnsupportedMime when `convert` is missing.
func renderDesign(ctx context.Context, mime string, src []byte) (image.Image, error) {
	inName, ok := designInputFilenameForMime[normalizeMime(mime)]
	if !ok {
		// Defence in depth: every MIME we register lives in the map
		// above. If a future contributor wires a renderer without
		// adding an entry, fall back to "in.bin" rather than crash
		// — ImageMagick will still try magic sniffing.
		inName = "in.bin"
	}
	// Layer our own timeout under the caller's context so we get
	// the tighter of (caller's deadline, designRenderTimeout).
	// Cancel on return so the subprocess is killed even on early
	// decode failure paths.
	renderCtx, cancel := context.WithTimeout(ctx, designRenderTimeout)
	defer cancel()
	return renderViaSubprocess(renderCtx, imagemagickBinary.Get(), inName, "out.png",
		[]string{"-density", "150", "{{in}}[0]", "-flatten", "-strip", "-resize", "600x600", "{{out}}"},
		src,
	)
}

func init() {
	// Register each MIME with a closure that captures the mime
	// string, mirroring the archive.go dispatch pattern. The
	// closure-per-mime shape lets the renderer pick the right
	// input filename extension per format — see
	// designInputFilenameForMime.
	for mime := range designInputFilenameForMime {
		mime := mime
		Register(RendererFunc(func(ctx context.Context, src []byte) (image.Image, error) {
			return renderDesign(ctx, mime, src)
		}), mime)
	}
	// CR2 / NEF / RAW family is left out by default — those need
	// per-vendor delegates that aren't part of a stock ImageMagick
	// package. Add per-deployment if needed.
}
