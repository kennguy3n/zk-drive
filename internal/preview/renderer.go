package preview

import (
	"context"
	"image"
	"sort"
	"strings"
	"sync"
)

// Renderer turns the raw source bytes of an uploaded file into an
// in-memory image suitable for further resize-and-encode steps. The
// service is the only caller; handlers MUST be safe for concurrent
// use across goroutines.
//
// Implementations should:
//   - respect the supplied context (deadline / cancellation),
//   - never modify the input slice (treat it as read-only),
//   - return ErrUnsupportedMime when an *expected* external dependency
//     is missing (e.g. LibreOffice not installed). The worker treats
//     ErrUnsupportedMime as a graceful skip so a misconfigured host
//     doesn't repeatedly Nak a job we cannot service.
//
// Any other returned error is treated as a hard failure by the worker
// (job is Nak'd and redelivered until the AckWait window expires).
type Renderer interface {
	// Render produces an in-memory image of the source bytes. The
	// returned image is consumed by the package-level resize +
	// PNG-encode steps, so handlers should produce the rasterised
	// representation at a reasonable working resolution (a few
	// hundred px on the long side is plenty given the 256 px
	// thumbnail target).
	Render(ctx context.Context, srcBytes []byte) (image.Image, error)
}

// RendererFunc adapts a plain function to the Renderer interface so
// per-format implementations can be wired with a tight `Register(...)`
// call rather than having to declare a one-method struct each time.
type RendererFunc func(ctx context.Context, srcBytes []byte) (image.Image, error)

// Render implements Renderer for RendererFunc.
func (f RendererFunc) Render(ctx context.Context, srcBytes []byte) (image.Image, error) {
	return f(ctx, srcBytes)
}

// registry is the package-level MIME-type → Renderer dispatch table.
// It is initialised by handler-specific files via Register in their
// init() functions, NOT by a centralised constructor — this keeps
// every format self-contained (its renderer, its tests, and its
// registration all live next to each other). Build-tag exclusion of
// a handler file is therefore automatically reflected in
// SupportedMimes().
//
// The map is guarded by a sync.RWMutex so tests can register / unset
// renderers without racing the worker's lookups. In production code
// path the mutex is read-only after init() so contention is nil.
var (
	registryMu sync.RWMutex
	registry   = map[string]Renderer{}
)

// Register associates a Renderer with each of the supplied MIME types.
// MIME types are normalised (lowercase, whitespace trimmed) so callers
// can be loose with capitalisation. Re-registration replaces the
// existing entry — this is intentional for tests that swap a real
// handler with a fake; production handlers never register the same
// MIME twice.
func Register(r Renderer, mimes ...string) {
	if r == nil {
		panic("preview: Register called with nil Renderer")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, m := range mimes {
		key := normalizeMime(m)
		if key == "" {
			panic("preview: Register called with empty MIME type")
		}
		registry[key] = r
	}
}

// Unregister removes the renderer for a MIME type, if any. Only used
// by tests that need to temporarily drop a handler (e.g. to assert
// the worker's "skip unsupported" path).
func Unregister(mime string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, normalizeMime(mime))
}

// lookup returns the renderer for a MIME type, or nil if none is
// registered. Internal — exported as IsSupportedMime / SupportedMimes
// for callers.
func lookup(mime string) Renderer {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[normalizeMime(mime)]
}

// IsSupportedMime reports whether the preview service can render a
// given mime type today. Backed by the renderer registry so it stays
// in sync with whatever handlers are compiled in.
func IsSupportedMime(mime string) bool {
	return lookup(mime) != nil
}

// SupportedMimes returns the sorted list of MIME types the preview
// service knows how to render in the current build. Useful for
// surfacing capability to the frontend ("Has preview?") and for
// logging / observability ("the worker boots with N supported
// formats").
func SupportedMimes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normalizeMime lowercases the MIME type, strips surrounding
// whitespace, and drops MIME parameters (anything after a ";"). The
// underlying file table's mime_type column stores the bare
// type/subtype today but the gateway is free to start storing a
// parameterised type ("text/plain; charset=utf-8") at any point;
// callers like the worker do not want to silently lose preview
// support when that happens. Stripping params here means a renderer
// registered for "text/plain" also serves "text/plain;
// charset=utf-8" without each handler having to opt in.
//
// We intentionally do NOT use mime.ParseMediaType: it allocates a
// map for the parameters we'd immediately throw away and rejects
// some non-conforming-but-real-world inputs. A single Cut at ";" is
// both faster and more permissive.
func normalizeMime(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// ErrUnsupportedDependencyMissing is the canonical error returned by
// a Renderer whose external binary (LibreOffice, ffmpeg, etc.) is
// not installed. It wraps ErrUnsupportedMime so callers can use
// errors.Is(err, ErrUnsupportedMime) for the catch-all "skip" path,
// while logs and metrics can still distinguish "format not wired"
// from "external tool missing".
var ErrUnsupportedDependencyMissing = errUnsupportedDep{}

type errUnsupportedDep struct{ msg string }

func (e errUnsupportedDep) Error() string {
	if e.msg == "" {
		return "preview: external dependency missing"
	}
	return "preview: external dependency missing: " + e.msg
}

func (errUnsupportedDep) Is(target error) bool {
	if target == nil {
		return false
	}
	if target == ErrUnsupportedMime {
		return true
	}
	_, ok := target.(errUnsupportedDep)
	return ok
}

// missingBinaryErr is a small helper for handlers: produce an error
// that is both descriptive (for logs) and matches both
// errors.Is(err, ErrUnsupportedMime) AND
// errors.Is(err, ErrUnsupportedDependencyMissing) so callers can
// branch on either.
func missingBinaryErr(name string) error {
	return errUnsupportedDep{msg: name + " not installed"}
}

// compile-time assertion that errUnsupportedDep matches the documented
// errors.Is semantics.
var _ error = errUnsupportedDep{}
var _ interface{ Is(error) bool } = errUnsupportedDep{}
