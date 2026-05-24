package preview

import (
	"image"
	"image/color"
	"strings"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/inconsolata"
	"golang.org/x/image/math/fixed"
)

// textPreviewOpts controls how renderTextToImage rasterises a chunk
// of text into an image. Defaults are tuned for "fits comfortably on
// a 600x600 working canvas that will then be resized to a 256 px
// thumbnail" — wide enough for ~80 columns of code, tall enough for
// ~30 lines of content.
type textPreviewOpts struct {
	// width / height of the rendered canvas in pixels. Leave 0 for
	// the defaults (600 x 600). The canvas is then scaled down to
	// ThumbnailSize by the service-level resize step, so picking a
	// larger working size preserves more detail in the final PNG.
	width, height int
	// maxLines caps the number of lines drawn. Long files are
	// truncated at this point and the final visible line is suffixed
	// with an ellipsis. 0 means "as many as fit".
	maxLines int
	// background and foreground colors. Defaults are an off-white
	// background (#F8F8F8) and a near-black foreground (#1F2933).
	// Pass a non-default fg to colourise headings / sections (see
	// archive.go which uses a slightly muted fg for the file-count
	// header).
	bg, fg color.Color
	// header is rendered in bold-ish (drawn twice with a 1 px x
	// offset) at the top of the canvas, separated from the body by a
	// 1 px horizontal rule. Empty string means "no header".
	header string
}

// renderTextToImage rasterises a body of text (plus an optional
// header) to a fixed-size image suitable for use as a preview.
//
// Wrapping is hard-cut at the canvas width — the rendered preview is
// a thumbnail, not a paginated reader, so doing accurate word-wrap
// would just spend cycles for no visible benefit at 256 px output. We
// do drop the body to maxLines visible lines and suffix an ellipsis
// on the last line when truncation happens, so the user can tell a
// preview is partial.
//
// The output image is always opaque RGBA so downstream resize +
// PNG-encode produces a deterministic byte stream.
func renderTextToImage(body string, opts textPreviewOpts) image.Image {
	if opts.width <= 0 {
		opts.width = 600
	}
	if opts.height <= 0 {
		opts.height = 600
	}
	if opts.bg == nil {
		opts.bg = color.RGBA{R: 0xF8, G: 0xF8, B: 0xF8, A: 0xFF}
	}
	if opts.fg == nil {
		opts.fg = color.RGBA{R: 0x1F, G: 0x29, B: 0x33, A: 0xFF}
	}

	face := inconsolata.Regular8x16
	metrics := face.Metrics()
	lineHeight := (metrics.Height + fixed.I(2)).Ceil() // small leading
	advance, _ := face.GlyphAdvance('M')               // monospace: every glyph has the same advance
	if advance == 0 {
		advance = fixed.I(8) // fallback if the font surprises us
	}
	colsPerLine := opts.width / advance.Ceil()
	if colsPerLine < 1 {
		colsPerLine = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, opts.width, opts.height))
	fillSolid(dst, opts.bg)

	drawer := &font.Drawer{Dst: dst, Src: solidImage(opts.fg), Face: face}

	x0 := 12
	y := lineHeight + 4
	maxY := opts.height - lineHeight/2

	if opts.header != "" {
		drawer.Dot = fixed.P(x0, y)
		drawer.DrawString(opts.header)
		// fake-bold by re-drawing 1 px to the right
		drawer.Dot = fixed.P(x0+1, y)
		drawer.DrawString(opts.header)
		// rule
		ruleY := y + 4
		if ruleY < dst.Bounds().Max.Y {
			drawHorizontalLine(dst, ruleY, opts.fg)
		}
		y = ruleY + lineHeight
	}

	lines := wrapLines(body, colsPerLine)
	maxLines := opts.maxLines
	// Cap visible lines to whatever fits on the canvas regardless of
	// the caller's request — a caller-supplied maxLines that exceeds
	// the canvas height would just paint off-canvas.
	canvasCap := (maxY - y) / lineHeight
	if canvasCap < 1 {
		canvasCap = 1
	}
	if maxLines <= 0 || maxLines > canvasCap {
		maxLines = canvasCap
	}
	truncated := len(lines) > maxLines
	if truncated {
		lines = lines[:maxLines]
	}
	lastIdx := len(lines) - 1
	for i, ln := range lines {
		if truncated && i == lastIdx {
			// trim the last line so the ellipsis fits within
			// the column budget. Slice by rune count, not byte
			// count — see truncateRunes for the rationale.
			trimTo := colsPerLine - 1
			if trimTo > 0 {
				ln = truncateRunes(ln, trimTo, "")
			}
			ln += "…"
		}
		drawer.Dot = fixed.P(x0, y)
		drawer.DrawString(ln)
		y += lineHeight
	}
	return dst
}

// wrapLines splits body on newlines and hard-cuts each physical line
// at width. Tabs are expanded to two spaces (a thumbnail-friendly
// width — keeping the source's 4 / 8 spaces would chew up the
// available columns very quickly).
//
// Width is interpreted as a count of RUNES (not bytes), because the
// monospace font we render with advances one cell per rune and the
// caller computes width from "how many cells fit in this canvas". A
// byte-based hard cut would split a multi-byte UTF-8 sequence in the
// middle and produce U+FFFD glyphs at wrap boundaries for any line
// with non-ASCII content. The rune-aware path costs one allocation
// per wrapped chunk in exchange for correct output on every script.
func wrapLines(body string, width int) []string {
	if width < 1 {
		width = 1
	}
	out := []string{}
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.ReplaceAll(strings.TrimRight(rawLine, "\r"), "\t", "  ")
		// Replace non-printable / control chars with spaces so a
		// binary blob masquerading as text doesn't break the renderer
		// or render a forest of "·" replacement glyphs.
		line = strings.Map(func(r rune) rune {
			if r == ' ' || unicode.IsPrint(r) {
				return r
			}
			return ' '
		}, line)
		if line == "" {
			out = append(out, "")
			continue
		}
		runes := []rune(line)
		for len(runes) > width {
			out = append(out, string(runes[:width]))
			runes = runes[width:]
		}
		out = append(out, string(runes))
	}
	return out
}

// truncateRunes returns s shortened to at most maxRunes runes,
// appending the suffix when truncation actually occurs. Returns s
// unchanged when its rune count is already <= maxRunes. This is the
// rune-aware analogue of `if len(s) > n { s = s[:n-1] + "…" }` and
// exists so callers don't have to remember to convert to []rune
// every time they slice a possibly-non-ASCII string. The suffix
// counts toward the visible width budget if the caller wants a
// strict cap; pass "" to skip suffixing.
func truncateRunes(s string, maxRunes int, suffix string) string {
	if maxRunes < 1 {
		return suffix
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	cut := maxRunes
	if suffix != "" {
		cut = maxRunes - len([]rune(suffix))
		if cut < 0 {
			cut = 0
		}
	}
	return string(runes[:cut]) + suffix
}

func fillSolid(dst *image.RGBA, c color.Color) {
	r, g, b, a := c.RGBA()
	col := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	for y := dst.Rect.Min.Y; y < dst.Rect.Max.Y; y++ {
		for x := dst.Rect.Min.X; x < dst.Rect.Max.X; x++ {
			dst.SetRGBA(x, y, col)
		}
	}
}

func solidImage(c color.Color) image.Image {
	return &image.Uniform{C: c}
}

func drawHorizontalLine(dst *image.RGBA, y int, c color.Color) {
	if y < dst.Rect.Min.Y || y >= dst.Rect.Max.Y {
		return
	}
	r, g, b, a := c.RGBA()
	col := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	for x := dst.Rect.Min.X; x < dst.Rect.Max.X; x++ {
		dst.SetRGBA(x, y, col)
	}
}
