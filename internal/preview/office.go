package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// sofficeBinary is the LibreOffice / OpenOffice headless command
// used to convert Office documents to PDF. Wrapped in binaryVar so
// Set + concurrent renderer reads are race-free — see binaryvar.go.
// Tests swap this out (mainly to exercise the "binary missing"
// graceful-skip path on CI hosts that don't ship LibreOffice).
var sofficeBinary = newBinaryVar("soffice")

// officeRenderTimeout is the per-document hard cap for the
// LibreOffice subprocess. LibreOffice is by far the heaviest
// preview backend — it loads a full office suite into memory, so
// 30s gives complex spreadsheets / decks enough time without
// letting a wedged process tie up a worker goroutine indefinitely.
// The caller's context still applies, so an even tighter deadline
// from upstream wins.
const officeRenderTimeout = 30 * time.Second

// renderOfficeDocument rasterises page 1 of a DOCX / XLSX / PPTX /
// ODT / ODS / ODP file by:
//
//  1. Converting the source to PDF via LibreOffice headless (writes
//     `<dir>/in.<ext>` and emits `<dir>/in.pdf`).
//  2. Rendering page 1 of that PDF with pdftoppm via
//     renderPDFFirstPage.
//
// LibreOffice (MPL-2.0) is shelled out, not linked, so it does not
// affect the proprietary build's licence. pdftoppm (poppler-utils,
// GPL) is similarly subprocess-only.
//
// Returns ErrUnsupportedMime when EITHER LibreOffice or pdftoppm is
// missing — both are required for the full pipeline.
func renderOfficeDocument(ctx context.Context, mime string, srcBytes []byte) (image.Image, error) {
	soffice := sofficeBinary.Get()
	if _, err := exec.LookPath(soffice); err != nil {
		return nil, missingBinaryErr("soffice")
	}
	if _, err := exec.LookPath(pdftoppmBinary.Get()); err != nil {
		return nil, missingBinaryErr("pdftoppm")
	}

	ext := officeExtensionFor(mime)
	if ext == "" {
		return nil, fmt.Errorf("%w: no extension mapping for %q", ErrUnsupportedMime, mime)
	}

	dir, err := os.MkdirTemp("", "zkdrive-office-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	inPath := filepath.Join(dir, "in."+ext)
	if err := os.WriteFile(inPath, srcBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write office source: %w", err)
	}

	convCtx, cancel := context.WithTimeout(ctx, officeRenderTimeout)
	defer cancel()
	cmd := exec.CommandContext(convCtx, soffice,
		"--headless",
		"--norestore",
		"--nologo",
		"--nofirststartwizard",
		"--nolockcheck",
		"--convert-to", "pdf",
		"--outdir", dir,
		inPath,
	)
	// LibreOffice writes user-profile state to $HOME. Pin it to the
	// per-invocation temp dir so concurrent worker goroutines don't
	// fight over a shared profile and so the state is cleaned up
	// with the rest of the dir on return.
	cmd.Env = append(os.Environ(), "HOME="+dir)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(convCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("soffice convert timed out after %s: %s", officeRenderTimeout, stderr.String())
		}
		return nil, fmt.Errorf("soffice convert: %w: %s", err, stderr.String())
	}

	pdfPath := filepath.Join(dir, "in.pdf")
	pdfBytes, err := os.ReadFile(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("read converted pdf: %w (stderr=%q)", err, stderr.String())
	}
	return renderPDFFirstPage(ctx, pdfBytes)
}

// officeExtensionFor maps a MIME type to the file extension we hand
// LibreOffice. LibreOffice sniffs by extension first, so handing it
// `in.docx` for a DOCX source — rather than `in.bin` — meaningfully
// improves its success rate on edge cases (e.g. legacy formats).
func officeExtensionFor(mime string) string {
	switch normalizeMime(mime) {
	// Microsoft Office (OOXML)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "pptx"
	// Microsoft Office legacy binary
	case "application/msword":
		return "doc"
	case "application/vnd.ms-excel":
		return "xls"
	case "application/vnd.ms-powerpoint":
		return "ppt"
	// OpenDocument (LibreOffice native)
	case "application/vnd.oasis.opendocument.text":
		return "odt"
	case "application/vnd.oasis.opendocument.spreadsheet":
		return "ods"
	case "application/vnd.oasis.opendocument.presentation":
		return "odp"
	// Rich Text Format. LibreOffice handles this fine and many email
	// systems attach .rtf as application/rtf or text/rtf.
	case "application/rtf", "text/rtf":
		return "rtf"
	}
	return ""
}

func init() {
	mimes := []string{
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/msword",
		"application/vnd.ms-excel",
		"application/vnd.ms-powerpoint",
		"application/vnd.oasis.opendocument.text",
		"application/vnd.oasis.opendocument.spreadsheet",
		"application/vnd.oasis.opendocument.presentation",
		"application/rtf",
		"text/rtf",
	}
	for _, m := range mimes {
		mime := m
		RegisterWeighted(WeightHeavy, RendererFunc(func(ctx context.Context, src []byte) (image.Image, error) {
			return renderOfficeDocument(ctx, mime, src)
		}), mime)
	}
}
