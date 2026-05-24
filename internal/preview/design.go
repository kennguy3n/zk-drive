package preview

import (
	"context"
	"image"
)

// imagemagickBinary is the ImageMagick `convert` command used to
// rasterise design / CAD-adjacent formats. Kept as a package-level
// var so tests can swap it. ImageMagick 7 ships `magick`; older
// versions ship `convert`. We default to `convert` for maximum
// compatibility with Debian / Alpine packages and let operators
// override it via SetImageMagickBinary at boot if their image only
// has the newer name.
var imagemagickBinary = "convert"

// SetImageMagickBinary lets ops or tests override the ImageMagick
// binary lookup. Exposed (vs. just being a package var) so deploys
// that bake ImageMagick 7 only — which ships `magick` and not
// `convert` — can flip it from a single configuration point at
// worker startup.
func SetImageMagickBinary(name string) { imagemagickBinary = name }

// renderDesign rasterises a design format (PSD, AI, EPS, TIFF) by
// shelling out to ImageMagick.
//
// We use Imagemagick's "first frame" semantics — `in[0]` — so a
// multi-layer PSD or a multi-page AI produces a single thumbnail
// instead of an animated montage. -flatten composites visible
// layers onto a single canvas; -strip drops metadata so the output
// PNG is small.
//
// Returns ErrUnsupportedMime when `convert` is missing.
func renderDesign(ctx context.Context, src []byte) (image.Image, error) {
	return renderViaSubprocess(ctx, imagemagickBinary, "in.bin", "out.png",
		[]string{"-density", "150", "{{in}}[0]", "-flatten", "-strip", "-resize", "600x600", "{{out}}"},
		src,
	)
}

func init() {
	mimes := []string{
		// Adobe
		"image/vnd.adobe.photoshop",
		"application/photoshop",
		"application/x-photoshop",
		"application/photoshop-document",
		"application/illustrator",
		"application/postscript",
		"application/eps",
		"image/x-eps",
		// TIFF — ImageMagick does a far better job on TIFF (including
		// CMYK and multi-page) than the stdlib decoder, which only
		// handles a narrow uncompressed subset.
		"image/tiff",
		"image/x-tiff",
		// BMP / ICO — stdlib can't decode these but ImageMagick can.
		"image/bmp",
		"image/x-bmp",
		"image/vnd.microsoft.icon",
		"image/x-icon",
		// HEIC / HEIF — phone-camera default on modern iOS. Needs
		// ImageMagick with the heif delegate, otherwise the
		// subprocess fails and the worker logs the error; we
		// intentionally do NOT special-case "delegate missing" as
		// ErrUnsupportedMime because most images we register here
		// CAN be rendered on a properly-built image.
		"image/heic",
		"image/heif",
		// CR2 / NEF / RAW family is left out by default — those need
		// per-vendor delegates that aren't part of a stock
		// ImageMagick package. Add per-deployment if needed.
	}
	Register(RendererFunc(renderDesign), mimes...)
}
