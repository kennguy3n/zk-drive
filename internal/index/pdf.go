package index

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// pdftotextBinary is the external binary used to extract plain text
// from a PDF. Kept as a package-level var so tests can swap it (e.g.
// point at a non-existent path to exercise the "not installed" branch),
// matching the pattern used in internal/preview/pdf.go for pdftoppm.
var pdftotextBinary = "pdftotext"

// extractPDFText shells out to pdftotext (poppler-utils, GPL — used
// as a subprocess, not linked, so it does not affect the proprietary
// build) to read the plain-text content of a PDF blob.
//
// The PDF is written to a temp file, pdftotext is asked to stream
// UTF-8 text to stdout (output path `-`), and the temp directory is
// removed before returning. The returned string is the raw extractor
// output with NO size cap applied here — the caller in
// ExtractTextWithContext is responsible for the final
// truncateUTF8(text, MaxIndexBytes) pass that pins the bytes written
// to files.content_text. Keeping the cap in the caller means every
// extractor branch (text/json/xml/pdf/docx) goes through one
// rune-boundary truncate site and the per-branch contract stays
// small.
//
// If pdftotext is not installed on the host, ErrUnsupportedMimeType
// is returned so the worker treats the job as a graceful skip —
// matching the contract every other ExtractText branch follows.
// Any other error (mkdir, write, decode, pdftotext exit code) is
// returned unchanged so a real failure does not silently masquerade
// as an unsupported-type ack.
func extractPDFText(ctx context.Context, pdfBytes []byte) (string, error) {
	if _, err := exec.LookPath(pdftotextBinary); err != nil {
		return "", fmt.Errorf("%w: pdftotext not installed", ErrUnsupportedMimeType)
	}

	dir, err := os.MkdirTemp("", "zkdrive-pdftext-*")
	if err != nil {
		return "", fmt.Errorf("index/pdf: mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	inPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, pdfBytes, 0o600); err != nil {
		return "", fmt.Errorf("index/pdf: write temp pdf: %w", err)
	}

	// `-` as the output path streams to stdout so we avoid a second
	// file read and a second cleanup site.
	// `-enc UTF-8` pins the output encoding so the truncateUTF8 pass
	// in ExtractText is operating on valid UTF-8 input.
	// `-nopgbrk` strips the U+000C form-feed pdftotext inserts
	// between pages — it is not useful for FTS and bloats the index.
	// `-q` silences benign warnings on stderr (corrupt-but-readable
	// PDFs are common in the wild).
	cmd := exec.CommandContext(ctx, pdftotextBinary,
		"-enc", "UTF-8",
		"-nopgbrk",
		"-q",
		inPath,
		"-",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("index/pdf: pdftotext: %w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}
