package preview

import (
	"context"
	"errors"
	"image"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegisterWeightedRoundTrip pins that an explicitly heavy
// registration is reported as heavy, a default Register is light, and
// Unregister clears both the renderer and its weight.
func TestRegisterWeightedRoundTrip(t *testing.T) {
	const heavyMime = "x-test/heavy-weight"
	const lightMime = "x-test/light-weight"
	Unregister(heavyMime)
	Unregister(lightMime)
	t.Cleanup(func() { Unregister(heavyMime); Unregister(lightMime) })

	stub := RendererFunc(func(_ context.Context, _ []byte) (image.Image, error) {
		return image.NewRGBA(image.Rect(0, 0, 1, 1)), nil
	})
	RegisterWeighted(WeightHeavy, stub, heavyMime)
	Register(stub, lightMime)

	if !IsHeavyMime(heavyMime) {
		t.Fatalf("IsHeavyMime(%q) = false, want true", heavyMime)
	}
	if IsHeavyMime(lightMime) {
		t.Fatalf("IsHeavyMime(%q) = true, want false", lightMime)
	}
	if got := WeightForMime(lightMime); got != WeightLight {
		t.Fatalf("WeightForMime(%q) = %v, want WeightLight", lightMime, got)
	}

	Unregister(heavyMime)
	if IsHeavyMime(heavyMime) {
		t.Fatalf("weight not cleared after Unregister(%q)", heavyMime)
	}
	if WeightForMime(heavyMime) != WeightLight {
		t.Fatalf("WeightForMime after Unregister should be the zero value WeightLight")
	}
}

// TestUnknownMimeIsLight confirms an unregistered MIME resolves to
// WeightLight, so a misclassified/unknown type is never routed to the
// scarce heavy pool.
func TestUnknownMimeIsLight(t *testing.T) {
	if IsHeavyMime("x-test/never-registered") {
		t.Fatalf("unknown MIME should not be heavy")
	}
}

// TestBuiltinHeavyClassification asserts the production renderers that
// shell out to a subprocess are all classified heavy, and the pure-Go
// renderers stay light. This is the contract the publish-side routing
// depends on: a wrong answer here sends a job to a pod that cannot run
// it.
func TestBuiltinHeavyClassification(t *testing.T) {
	heavy := []string{
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", // LibreOffice
		"application/pdf",           // pdftoppm
		"image/svg+xml",             // rsvg-convert
		"audio/mpeg",                // ffmpeg
		"video/mp4",                 // ffmpeg
		"image/vnd.adobe.photoshop", // ImageMagick (design)
	}
	for _, m := range heavy {
		if !IsSupportedMime(m) {
			t.Fatalf("expected built-in renderer for %q", m)
		}
		if !IsHeavyMime(m) {
			t.Fatalf("MIME %q should be heavy (subprocess renderer)", m)
		}
	}

	light := []string{
		"image/png",
		"image/jpeg",
		"text/plain",
		"text/markdown",
		"text/csv",
	}
	for _, m := range light {
		if !IsSupportedMime(m) {
			t.Fatalf("expected built-in renderer for %q", m)
		}
		if IsHeavyMime(m) {
			t.Fatalf("MIME %q should be lightweight (pure-Go renderer)", m)
		}
	}

	// HeavyMimes must contain only heavy entries and at least the
	// office document above.
	found := false
	for _, m := range HeavyMimes() {
		if !IsHeavyMime(m) {
			t.Fatalf("HeavyMimes returned %q which is not heavy", m)
		}
		if m == "application/pdf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("HeavyMimes missing application/pdf")
	}
}

// TestSubprocessGateBounds verifies the concurrency gate admits exactly
// the configured number of slots concurrently and blocks the next
// acquirer until a slot is released.
func TestSubprocessGateBounds(t *testing.T) {
	SetSubprocessConcurrency(2)
	t.Cleanup(func() { SetSubprocessConcurrency(0) })

	if got := SubprocessConcurrency(); got != 2 {
		t.Fatalf("SubprocessConcurrency() = %d, want 2", got)
	}

	ctx := context.Background()
	rel1, err := acquireSubprocessSlot(ctx)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	rel2, err := acquireSubprocessSlot(ctx)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	// Third acquire must block until a slot frees. Probe with a short
	// ctx deadline: it should time out while both slots are held.
	probeCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := acquireSubprocessSlot(probeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third acquire should block (got err=%v)", err)
	}

	// Release one slot, then the third acquire must succeed promptly.
	rel1()
	rel3, err := acquireSubprocessSlot(ctx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	rel2()
	rel3()
}

// TestSubprocessGateReleaseIdempotent confirms calling release twice
// frees only one slot (the second call is a no-op), so a defer + manual
// release double-call cannot corrupt the semaphore count.
func TestSubprocessGateReleaseIdempotent(t *testing.T) {
	SetSubprocessConcurrency(1)
	t.Cleanup(func() { SetSubprocessConcurrency(0) })

	rel, err := acquireSubprocessSlot(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()
	rel() // second call must be a no-op, not free a phantom slot

	// Only one slot exists; acquire it, then a probe must block.
	relA, err := acquireSubprocessSlot(context.Background())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	defer relA()
	probeCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := acquireSubprocessSlot(probeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slot count corrupted: expected block, got err=%v", err)
	}
}

// TestSubprocessGateDisabledNoOp confirms a zero/negative cap makes
// acquire a non-blocking no-op with a harmless release.
func TestSubprocessGateDisabledNoOp(t *testing.T) {
	SetSubprocessConcurrency(0)
	if got := SubprocessConcurrency(); got != 0 {
		t.Fatalf("SubprocessConcurrency() = %d, want 0 (disabled)", got)
	}
	// Many concurrent acquires must all succeed immediately.
	var wg sync.WaitGroup
	var acquired atomic.Int64
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := acquireSubprocessSlot(context.Background())
			if err != nil {
				t.Errorf("acquire on disabled gate: %v", err)
				return
			}
			acquired.Add(1)
			rel()
		}()
	}
	wg.Wait()
	if acquired.Load() != 64 {
		t.Fatalf("acquired %d, want 64 (disabled gate must never block)", acquired.Load())
	}
}
