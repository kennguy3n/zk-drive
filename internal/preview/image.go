package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder
	_ "image/jpeg" // register JPEG decoder
	_ "image/png"  // register PNG decoder

	// register the WebP decoder. The golang.org/x/image/webp
	// package is pure-Go (no cgo) and already a transitive
	// dependency via golang.org/x/image, so wiring WebP costs us
	// effectively zero — and image/webp is increasingly common as
	// browsers and editors emit it by default.
	_ "golang.org/x/image/webp"
)

// renderRasterImage decodes a raster image with the stdlib image
// package (extended with the WebP decoder via the side-effect import
// above). This is the in-process, dependency-free renderer used for
// every format that Go's standard library + x/image can decode
// without shelling out.
//
// Errors are returned verbatim — image.Decode does not produce
// ErrUnsupportedMime, so callers MUST only register this renderer
// against MIME types image.Decode is known to handle. Decode failures
// here are real corruption signals (not "format not supported"),
// which is why we do NOT map them to ErrUnsupportedMime.
func renderRasterImage(_ context.Context, srcBytes []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(srcBytes))
	if err != nil {
		return nil, fmt.Errorf("decode source image: %w", err)
	}
	return img, nil
}

func init() {
	// MIME types that the stdlib image package + x/image/webp can
	// decode directly. image/jpg is a non-standard alias that some
	// uploads still carry (the canonical is image/jpeg); we register
	// both so a misspelt Content-Type from the client doesn't lose
	// previewability.
	Register(RendererFunc(renderRasterImage),
		"image/png",
		"image/jpeg",
		"image/jpg",
		"image/gif",
		"image/webp",
	)
}
