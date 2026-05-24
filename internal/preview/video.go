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
// Kept as a package-level var so tests can swap it out.
var ffmpegBinary = "ffmpeg"

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
	if _, err := exec.LookPath(ffmpegBinary); err != nil {
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

	runCtx, cancel := context.WithTimeout(ctx, videoRenderTimeout)
	defer cancel()
	// -ss before -i is the fast seek path: ffmpeg jumps directly to
	// the closest keyframe before videoFrameTimeOffset, which is
	// much faster than decoding from t=0. -frames:v 1 + -an drops
	// audio entirely so the encoder doesn't waste time on it.
	// -loglevel error keeps stderr quiet on the happy path; we
	// surface it on errors.
	cmd := exec.CommandContext(runCtx, ffmpegBinary,
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		if img, decErr := readImageFile(outPath); decErr == nil {
			return img, nil
		}
	}
	// Fallback path: some short clips don't have a frame at t=1s
	// because the whole file is <1s, ffmpeg's fast-seek skipped past
	// the only available keyframe, etc. Retry without -ss so ffmpeg
	// picks the very first frame regardless of timestamp.
	stderr.Reset()
	if outErr := os.Remove(outPath); outErr != nil && !errors.Is(outErr, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale frame: %w", outErr)
	}
	cmd = exec.CommandContext(runCtx, ffmpegBinary,
		"-y",
		"-i", inPath,
		"-frames:v", "1",
		"-an",
		"-vcodec", "png",
		"-loglevel", "error",
		outPath,
	)
	cmd.Dir = dir
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("ffmpeg video frame timed out after %s: %s", videoRenderTimeout, stderr.String())
		}
		return nil, fmt.Errorf("ffmpeg video frame: %w: %s", err, stderr.String())
	}
	img, err := readImageFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("decode video frame: %w (stderr=%q)", err, stderr.String())
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
	Register(RendererFunc(renderVideoFrame), mimes...)
}
