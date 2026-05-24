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

// audioRenderTimeout caps waveform generation. Decoding audio is
// generally fast, but compressed long-form audio (a 3-hour podcast)
// can stretch ffmpeg out — 20s is a comfortable upper bound.
const audioRenderTimeout = 20 * time.Second

// audioWaveformBinary is the optional `audiowaveform` (BBC) command
// used to render a high-quality waveform PNG. If present we prefer
// it (sharper output, more accurate peak detection). If absent we
// fall back to ffmpeg's `showwavespic` filter, which produces an
// acceptable waveform without an extra runtime dependency.
//
// Both are package-level vars so tests can swap them out.
var (
	audioWaveformBinary = "audiowaveform"
	audioWaveformWidth  = "800"
	audioWaveformHeight = "200"
)

// renderAudioWaveform produces a PNG waveform thumbnail for an
// audio file. It tries audiowaveform first (preferred when
// available), then falls back to ffmpeg's showwavespic filter.
//
// Returns ErrUnsupportedMime when NEITHER tool is installed — both
// are external; either is sufficient. The fallback chain keeps
// developer workstations productive (most have ffmpeg from the
// video pipeline) while still letting production prefer
// audiowaveform when the worker image carries it.
func renderAudioWaveform(ctx context.Context, srcBytes []byte) (image.Image, error) {
	if _, err := exec.LookPath(audioWaveformBinary); err == nil {
		img, err := renderAudioWaveformWithBBC(ctx, srcBytes)
		if err == nil {
			return img, nil
		}
		// BBC tool failed (not "missing", actually failed). Fall
		// through to ffmpeg — corrupt input is one thing, but a
		// BBC-specific codec gap shouldn't lose the preview.
	}
	if _, err := exec.LookPath(ffmpegBinary); err == nil {
		return renderAudioWaveformWithFFmpeg(ctx, srcBytes)
	}
	return nil, missingBinaryErr("audiowaveform or ffmpeg")
}

func renderAudioWaveformWithBBC(ctx context.Context, srcBytes []byte) (image.Image, error) {
	dir, err := os.MkdirTemp("", "zkdrive-audio-bbc-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	inPath := filepath.Join(dir, "in.audio")
	outPath := filepath.Join(dir, "out.png")
	if err := os.WriteFile(inPath, srcBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write audio source: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, audioRenderTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, audioWaveformBinary,
		"-i", inPath,
		"-o", outPath,
		"--pixels-per-second", "100",
		"-w", audioWaveformWidth,
		"-h", audioWaveformHeight,
		"--background-color", "F8F8F8",
		"--waveform-color", "1F2933",
		"--no-axis-labels",
	)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("audiowaveform timed out after %s: %s", audioRenderTimeout, stderr.String())
		}
		return nil, fmt.Errorf("audiowaveform: %w: %s", err, stderr.String())
	}
	return readImageFile(outPath)
}

func renderAudioWaveformWithFFmpeg(ctx context.Context, srcBytes []byte) (image.Image, error) {
	dir, err := os.MkdirTemp("", "zkdrive-audio-ffmpeg-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	inPath := filepath.Join(dir, "in.audio")
	outPath := filepath.Join(dir, "out.png")
	if err := os.WriteFile(inPath, srcBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write audio source: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, audioRenderTimeout)
	defer cancel()
	// showwavespic renders the entire decoded waveform onto a
	// fixed-size canvas. colors=#1F2933 matches the BBC fallback
	// foreground for consistency. The filter accepts a single
	// colour at most; the canvas defaults to transparent, which we
	// flatten over an off-white background via overlay so PNGs
	// look uniform between the two backends.
	cmd := exec.CommandContext(runCtx, ffmpegBinary,
		"-y",
		"-i", inPath,
		"-filter_complex", "color=c=0xF8F8F8:s="+audioWaveformWidth+"x"+audioWaveformHeight+"[bg];[0:a]showwavespic=s="+audioWaveformWidth+"x"+audioWaveformHeight+":colors=0x1F2933[fg];[bg][fg]overlay=format=auto",
		"-frames:v", "1",
		"-loglevel", "error",
		outPath,
	)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("ffmpeg waveform timed out after %s: %s", audioRenderTimeout, stderr.String())
		}
		return nil, fmt.Errorf("ffmpeg waveform: %w: %s", err, stderr.String())
	}
	return readImageFile(outPath)
}

func init() {
	mimes := []string{
		"audio/mpeg",
		"audio/mp3",
		"audio/mp4",
		"audio/wav",
		"audio/x-wav",
		"audio/wave",
		"audio/flac",
		"audio/x-flac",
		"audio/ogg",
		"audio/vorbis",
		"audio/opus",
		"audio/aac",
		"audio/x-m4a",
		"audio/webm",
		"audio/amr",
	}
	Register(RendererFunc(renderAudioWaveform), mimes...)
}
