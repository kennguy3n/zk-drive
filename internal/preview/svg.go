package preview

import (
	"context"
	"image"
	"time"
)

// rsvgBinary is the librsvg `rsvg-convert` command used to rasterise
// SVG documents. Kept as a package-level var so tests can swap it.
var rsvgBinary = "rsvg-convert"

// svgRenderTimeout caps a single rsvg-convert invocation. SVG
// rasterisation is normally fast (single-digit ms) but a pathological
// document (deeply nested groups, a filter chain that produces a
// huge intermediate image) can stretch rsvg-convert for tens of
// seconds. Without this cap, the only bound is the worker-level
// 2-minute job timeout — which is generous enough that a single
// runaway SVG can monopolise a worker goroutine and starve
// everything else. 15s matches the video frame budget.
const svgRenderTimeout = 15 * time.Second

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
	// Layer our own timeout under the caller's context so we get
	// the tighter of (caller's deadline, svgRenderTimeout). Cancel
	// on return so the subprocess is killed even on early decode
	// failure paths.
	renderCtx, cancel := context.WithTimeout(ctx, svgRenderTimeout)
	defer cancel()
	return renderViaSubprocess(renderCtx, rsvgBinary, "in.svg", "out.png",
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
