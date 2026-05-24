package preview

import (
	"context"
	"image"
)

// rsvgBinary is the librsvg `rsvg-convert` command used to rasterise
// SVG documents. Kept as a package-level var so tests can swap it.
var rsvgBinary = "rsvg-convert"

// renderSVG rasterises an SVG document to a PNG and decodes it.
// librsvg (LGPL) is shelled out as `rsvg-convert`, not linked, so
// it does not affect the proprietary build's licence.
//
// We rasterise at 600 px wide to give the downstream resize step
// enough resolution to produce a crisp 256 px thumbnail. Height is
// preserved by rsvg-convert so the aspect ratio matches the
// source.
//
// Returns ErrUnsupportedMime when rsvg-convert is missing.
func renderSVG(ctx context.Context, src []byte) (image.Image, error) {
	return renderViaSubprocess(ctx, rsvgBinary, "in.svg", "out.png",
		[]string{"-w", "600", "-f", "png", "-o", "{{out}}", "{{in}}"},
		src,
	)
}

func init() {
	Register(RendererFunc(renderSVG),
		"image/svg+xml",
		"image/svg",
	)
}
