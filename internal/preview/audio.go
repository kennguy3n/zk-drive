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
// audioWaveformBinary is wrapped in binaryVar so Set + concurrent
// renderer reads are race-free — see binaryvar.go. The width/height
// strings stay as plain strings because they are init-only configuration
// (no exposed setter, no runtime swap path).
var audioWaveformBinary = newBinaryVar("audiowaveform")

var (
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
	var bbcErr error
	if _, err := exec.LookPath(audioWaveformBinary.Get()); err == nil {
		img, runErr := renderAudioWaveformWithBBC(ctx, srcBytes)
		if runErr == nil {
			return img, nil
		}
		bbcErr = runErr
		// Don't fall through if the caller's context is already
		// done — ffmpeg would just fail immediately for the same
		// reason and the resulting error message would point at
		// ffmpeg, hiding that the cancellation actually killed
		// the BBC attempt. Returning the BBC error here preserves
		// the real cause and matches what callers expect from a
		// context-cancelled run.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("audiowaveform: %w", bbcErr)
		}
		// BBC tool failed (not "missing", actually failed) and
		// the caller still has time on the clock. Fall through to
		// ffmpeg — corrupt input is one thing, but a BBC-specific
		// codec gap shouldn't lose the preview.
	}
	if _, err := exec.LookPath(ffmpegBinary.Get()); err == nil {
		img, ffErr := renderAudioWaveformWithFFmpeg(ctx, srcBytes)
		if ffErr != nil && bbcErr != nil {
			// Surface BOTH failures so operators investigating an
			// audio preview outage can tell whether the BBC tool
			// or ffmpeg is the actual culprit.
			return nil, fmt.Errorf("audio waveform: ffmpeg failed: %w (audiowaveform also failed: %v)", ffErr, bbcErr)
		}
		return img, ffErr
	}
	if bbcErr != nil {
		// We attempted the BBC tool, it failed, and ffmpeg isn't
		// installed. Surface the BBC error rather than the
		// generic "neither installed" message; the caller's diagnostic
		// is "what actually went wrong on the one available tool".
		return nil, fmt.Errorf("audiowaveform: %w", bbcErr)
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
	cmd := exec.CommandContext(runCtx, audioWaveformBinary.Get(),
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
	cmd := exec.CommandContext(runCtx, ffmpegBinary.Get(),
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
	RegisterWeighted(WeightHeavy, RendererFunc(renderAudioWaveform), mimes...)
}
