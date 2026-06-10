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

// ffmpegBinary is the FFmpeg command used to extract a video frame.
// Wrapped in binaryVar so Set + concurrent renderer reads are
// race-free — see binaryvar.go.
var ffmpegBinary = newBinaryVar("ffmpeg")

// videoFrameTimeOffset is the timestamp at which we grab a single
// frame from the video. t=1s is conventional in video-thumbnail
// pipelines: it skips any black-frame intro, gives enough material
// for the encoder to produce a meaningful image, and falls within
// the first GOP of essentially every codec.
const videoFrameTimeOffset = "00:00:01"

// videoRenderTimeout caps the ffmpeg subprocess at 15 s. Frame
// extraction is much cheaper than office conversion, but a wedged
// codec (e.g. a corrupt mp4 that ffmpeg can't seek past) shouldn't
// be allowed to tie up a worker indefinitely.
const videoRenderTimeout = 15 * time.Second

// renderVideoFrame extracts a single frame near t=1s from a video
// file via ffmpeg. The frame is written as PNG to a temp file and
// then decoded with the stdlib image package.
//
// FFmpeg (LGPL when built without GPL components, otherwise GPL) is
// shelled out, not linked, so it does not affect the proprietary
// build's licence.
//
// If ffmpeg is not installed on the host, ErrUnsupportedMime is
// returned so the worker treats the job as a graceful skip.
func renderVideoFrame(ctx context.Context, srcBytes []byte) (image.Image, error) {
	ffmpeg := ffmpegBinary.Get()
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return nil, missingBinaryErr("ffmpeg")
	}

	dir, err := os.MkdirTemp("", "zkdrive-video-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	inPath := filepath.Join(dir, "in")
	outPath := filepath.Join(dir, "out.png")
	if err := os.WriteFile(inPath, srcBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write video source: %w", err)
	}

	// Each ffmpeg attempt gets its OWN videoRenderTimeout budget,
	// not a shared one. A slow-but-not-dead primary attempt (e.g.
	// fast-seek on a slightly damaged mp4) used to consume the
	// shared budget and leave the fallback no time to run; giving
	// each attempt a fresh timeout makes the retry actually
	// useful. The caller's ctx still bounds the total wall time
	// via the worker's job-level deadline, so this can't run away.
	//
	// Each attempt gets its own stderr buffer because we want to
	// preserve the primary attempt's diagnostics when the fallback
	// also fails — discarding the primary stderr made debugging
	// fast-seek-only failures (the most common ffmpeg edge case)
	// effectively impossible.
	var primaryStderr, fallbackStderr bytes.Buffer
	primaryCtx, primaryCancel := context.WithTimeout(ctx, videoRenderTimeout)
	defer primaryCancel()
	// -ss before -i is the fast seek path: ffmpeg jumps directly to
	// the closest keyframe before videoFrameTimeOffset, which is
	// much faster than decoding from t=0. -frames:v 1 + -an drops
	// audio entirely so the encoder doesn't waste time on it.
	// -loglevel error keeps stderr quiet on the happy path; we
	// surface it on errors.
	cmd := exec.CommandContext(primaryCtx, ffmpeg,
		"-y",
		"-ss", videoFrameTimeOffset,
		"-i", inPath,
		"-frames:v", "1",
		"-an",
		"-vcodec", "png",
		"-loglevel", "error",
		outPath,
	)
	cmd.Dir = dir
	cmd.Stderr = &primaryStderr
	var (
		primaryRunErr    error
		primaryDecodeErr error
	)
	if primaryRunErr = cmd.Run(); primaryRunErr == nil {
		// ffmpeg exited 0 but the PNG it wrote may still be
		// undecodable (rare, but happens with some codecs that
		// produce a 0-byte or truncated output despite a clean
		// exit code). Capture the decode error so it appears in
		// the final diagnostic when the fallback also fails —
		// otherwise operators would see "primary err=<nil>" with
		// no hint that decode was actually the failure mode.
		var img image.Image
		img, primaryDecodeErr = readImageFile(outPath)
		if primaryDecodeErr == nil {
			return img, nil
		}
	}
	// Fallback path: some short clips don't have a frame at t=1s
	// because the whole file is <1s, ffmpeg's fast-seek skipped past
	// the only available keyframe, etc. Retry without -ss so ffmpeg
	// picks the very first frame regardless of timestamp.
	if outErr := os.Remove(outPath); outErr != nil && !errors.Is(outErr, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale frame: %w", outErr)
	}
	fallbackCtx, fallbackCancel := context.WithTimeout(ctx, videoRenderTimeout)
	defer fallbackCancel()
	cmd = exec.CommandContext(fallbackCtx, ffmpeg,
		"-y",
		"-i", inPath,
		"-frames:v", "1",
		"-an",
		"-vcodec", "png",
		"-loglevel", "error",
		outPath,
	)
	cmd.Dir = dir
	cmd.Stderr = &fallbackStderr
	if err := cmd.Run(); err != nil {
		// Surface BOTH attempts' diagnostics when we fail. The
		// primary attempt is the most common ffmpeg-bug culprit
		// (fast-seek on damaged containers, codec quirks), so
		// hiding its stderr made historical incident reports
		// useless. The primaryRunErr (if any) is also included
		// so operators can see whether the fallback was triggered
		// by a real error or by a decode-failure after a
		// successful primary run.
		if errors.Is(fallbackCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf(
				"ffmpeg video frame timed out after %s on fallback (primary err=%v, primary decode err=%v, primary stderr=%q): %s",
				videoRenderTimeout, primaryRunErr, primaryDecodeErr, primaryStderr.String(), fallbackStderr.String(),
			)
		}
		return nil, fmt.Errorf(
			"ffmpeg video frame: %w (primary err=%v, primary decode err=%v, primary stderr=%q, fallback stderr=%q)",
			err, primaryRunErr, primaryDecodeErr, primaryStderr.String(), fallbackStderr.String(),
		)
	}
	img, err := readImageFile(outPath)
	if err != nil {
		return nil, fmt.Errorf(
			"decode video frame: %w (primary err=%v, primary decode err=%v, primary stderr=%q, fallback stderr=%q)",
			err, primaryRunErr, primaryDecodeErr, primaryStderr.String(), fallbackStderr.String(),
		)
	}
	return img, nil
}

func init() {
	mimes := []string{
		"video/mp4",
		"video/quicktime",
		"video/x-matroska",
		"video/webm",
		"video/x-msvideo",
		"video/x-ms-wmv",
		"video/mpeg",
		"video/3gpp",
		"video/3gpp2",
		"video/ogg",
		"video/x-flv",
	}
	RegisterWeighted(WeightHeavy, RendererFunc(renderVideoFrame), mimes...)
}
