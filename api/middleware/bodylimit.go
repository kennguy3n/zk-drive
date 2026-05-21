package middleware

import "net/http"

// DefaultMaxBodyBytes is the standard request-body cap mounted globally
// on every API route. 1 MiB is well above any expected JSON payload
// (workspace settings, share-link configs, batch IDs) but small enough
// that a single misbehaving client cannot exhaust process memory by
// streaming a multi-gigabyte body before being rejected.
//
// Routes that legitimately accept larger payloads (none today; file
// bytes flow through presigned URLs that bypass the API entirely) may
// scope a larger limit per-route via MaxBodySize. Routes that need a
// smaller limit (e.g. the Stripe webhook at 64 KiB) wrap the body a
// second time inside their handler — http.MaxBytesReader composes
// correctly, the tighter inner limit wins.
const DefaultMaxBodyBytes = int64(1 << 20) // 1 MiB

// MaxBodySize returns a middleware that caps the request body at
// maxBytes. Once the body exceeds the cap, subsequent reads return
// *http.MaxBytesError and the underlying ResponseWriter is automatically
// set to 413 Request Entity Too Large.
//
// The wrap is a no-op for requests without a body (Method == GET /
// HEAD / DELETE / OPTIONS / WebSocket upgrades), which means GET
// handlers and the /api/ws upgrade path are unaffected.
//
// Negative or zero maxBytes is treated as "no limit" — the body is
// passed through unchanged. This keeps the helper usable in tests
// that want to disable the cap without a separate code path.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 && r.Body != nil && bodyMayHavePayload(r.Method) {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bodyMayHavePayload reports whether the request method is one that
// the HTTP spec defines as carrying a body. GET / HEAD / DELETE /
// OPTIONS technically *can* carry a body, but in practice they don't
// in zk-drive's API surface — and wrapping their nil body would still
// be safe but a wasted allocation. Restricting the wrap to mutating
// verbs keeps the middleware cheap on the hot read path.
func bodyMayHavePayload(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}
