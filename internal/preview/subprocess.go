package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/png" // registers the PNG decoder used by readImageFile
	"os"
	"os/exec"
	"path/filepath"
)

// renderViaSubprocess is a shared helper for handlers whose external
// tool follows the same shape:
//
//  1. Create a temp dir.
//  2. Write the source bytes to <dir>/<inName>.
//  3. Spawn <binary> with args that produce an image file at
//     <dir>/<outName>.
//  4. Decode the produced image file (PNG/JPEG/etc.) and return it.
//
// `binary` is looked up on PATH; if missing, we return a wrapped
// ErrUnsupportedMime so the worker treats the format as a graceful
// skip rather than a hard error that retries forever. The `args` list
// MUST contain {{in}} and {{out}} placeholders which the helper
// substitutes with the actual temp paths — this keeps the per-handler
// call site readable instead of having every handler reinvent path
// joining + temp-file plumbing.
//
// The temp directory is cleaned up unconditionally via defer.
func renderViaSubprocess(ctx context.Context, binary, inName, outName string, args []string, srcBytes []byte) (image.Image, error) {
	if _, err := exec.LookPath(binary); err != nil {
		return nil, missingBinaryErr(binary)
	}

	dir, err := os.MkdirTemp("", "zkdrive-preview-"+filepath.Base(binary)+"-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	inPath := filepath.Join(dir, inName)
	outPath := filepath.Join(dir, outName)
	if err := os.WriteFile(inPath, srcBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write temp source: %w", err)
	}

	rendered := make([]string, len(args))
	for i, a := range args {
		switch a {
		case "{{in}}":
			rendered[i] = inPath
		case "{{out}}":
			rendered[i] = outPath
		case "{{dir}}":
			rendered[i] = dir
		default:
			rendered[i] = a
		}
	}
	cmd := exec.CommandContext(ctx, binary, rendered...)
	cmd.Dir = dir
	// LibreOffice in particular insists on writing config to $HOME;
	// pinning HOME to the per-invocation temp dir prevents collisions
	// between concurrent worker goroutines and keeps state hermetic
	// (cleaned up by RemoveAll). This is a no-op for tools that
	// ignore $HOME.
	cmd.Env = append(os.Environ(), "HOME="+dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", binary, err, stderr.String())
	}

	img, err := readImageFile(outPath)
	if err != nil {
		// If the tool succeeded but produced no file at the expected
		// path, fall back to scanning the temp dir for a file that
		// the tool may have named differently (e.g. LibreOffice
		// derives output names from the input). This avoids handlers
		// having to know the tool's naming convention exactly.
		alt, altErr := scanDirForImage(dir, outPath)
		if altErr != nil {
			return nil, fmt.Errorf("read %s output: %w (scan fallback: %v) stderr=%q", binary, err, altErr, stderr.String())
		}
		return alt, nil
	}
	return img, nil
}

func readImageFile(path string) (image.Image, error) {
	f, err := os.Open(path) // #nosec G304 — path comes from a per-invocation temp dir owned by us
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return img, nil
}

// scanDirForImage walks a temp dir for the first decodable image
// file. Used as a fallback when a subprocess wrote its output under a
// name we couldn't predict from the input (LibreOffice picks the
// extension based on the requested filter; ImageMagick may add a
// frame suffix; etc.). The skip parameter lets the caller ignore the
// path it already tried.
func scanDirForImage(dir, skip string) (image.Image, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if p == skip {
			continue
		}
		if img, err := readImageFile(p); err == nil {
			return img, nil
		}
	}
	return nil, fmt.Errorf("no image file found in %s", dir)
}
