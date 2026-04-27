package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
)

// pdftoppmBinary is the external binary used to rasterise a PDF page.
// Kept as a package-level var so tests can swap it (e.g. point at a
// non-existent path to exercise the "not installed" branch).
var pdftoppmBinary = "pdftoppm"

// renderPDFFirstPage rasterises page 1 of a PDF blob to an image by
// shelling out to pdftoppm (poppler-utils, GPL — used as a subprocess,
// not linked, so it does not affect the proprietary build).
//
// The PDF is written to a temp file, pdftoppm produces a PNG next to
// it, and the PNG is decoded with the stdlib image package. Both temp
// files are removed before returning.
//
// If pdftoppm is not installed on the host, ErrUnsupportedMime is
// returned so the worker treats the job as a graceful skip rather
// than a hard failure.
func renderPDFFirstPage(ctx context.Context, pdfBytes []byte) (image.Image, error) {
	if _, err := exec.LookPath(pdftoppmBinary); err != nil {
		return nil, fmt.Errorf("%w: pdftoppm not installed", ErrUnsupportedMime)
	}

	dir, err := os.MkdirTemp("", "zkdrive-pdf-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, pdfBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write temp pdf: %w", err)
	}

	outPrefix := filepath.Join(dir, "out")
	cmd := exec.CommandContext(ctx, pdftoppmBinary,
		"-png",
		"-f", "1",
		"-l", "1",
		"-r", "150",
		"-singlefile",
		inPath,
		outPrefix,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %w: %s", err, stderr.String())
	}

	pngPath := outPrefix + ".png"
	pngBytes, err := os.ReadFile(pngPath)
	if err != nil {
		return nil, fmt.Errorf("read pdftoppm output: %w", err)
	}

	img, _, err := image.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode pdftoppm output: %w", err)
	}
	return img, nil
}
