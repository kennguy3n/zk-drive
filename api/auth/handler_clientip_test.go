package auth

import (
	"net/http"
	"testing"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/session"
)

// TestClientIPMatchesMiddlewareResolution pins the security-critical
// invariant behind the 6.2 device-anomaly check: the client IP the
// auth handler resolves at login (and folds into the stored device
// fingerprint) MUST be byte-identical to the IP AuthMiddleware resolves
// on every subsequent request for the same depth. Both delegate to
// middleware.ClientIPFromRequest, so this test fails the moment the
// handler's resolution diverges from the middleware's (e.g. a refactor
// that stops honouring trustedProxyDepth or reads a different header) —
// the precise drift that would otherwise make the fingerprint captured
// at login never match the one recomputed per request and reject every
// authenticated request as an anomaly.
func TestClientIPMatchesMiddlewareResolution(t *testing.T) {
	newReq := func(remoteAddr string, xff ...string) *http.Request {
		r := &http.Request{
			RemoteAddr: remoteAddr,
			Header:     http.Header{},
		}
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}

	tests := []struct {
		name  string
		depth int
		req   *http.Request
	}{
		{"no_proxy_remote_addr", 0, newReq("203.0.113.7:54321")},
		{"depth0_ignores_xff", 0, newReq("203.0.113.7:54321", "198.51.100.9")},
		{"depth1_single_xff", 1, newReq("10.0.0.1:443", "198.51.100.9")},
		{"depth1_multi_hop_xff", 1, newReq("10.0.0.1:443", "198.51.100.9, 198.51.100.10")},
		{"depth2_multi_hop_xff", 2, newReq("10.0.0.1:443", "198.51.100.9, 198.51.100.10")},
		{"depth_exceeds_hops_falls_back", 3, newReq("10.0.0.1:443", "198.51.100.9")},
		{"ipv6_remote_addr", 0, newReq("[2001:db8::1]:8443")},
		{"malformed_xff_falls_back", 1, newReq("203.0.113.7:54321", "not-an-ip")},
		{"unparseable_remote_addr", 0, newReq("garbage")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{trustedProxyDepth: tc.depth}

			// What the login flow stores in the fingerprint.
			handlerIP := h.clientIP(tc.req)

			// What AuthMiddleware recomputes per request.
			middlewareIP := ""
			if ip := middleware.ClientIPFromRequest(tc.req, tc.depth); ip != nil {
				middlewareIP = ip.String()
			}

			if handlerIP != middlewareIP {
				t.Fatalf("handler clientIP %q != middleware-resolved IP %q (depth %d): "+
					"login fingerprint would never match the per-request recomputation",
					handlerIP, middlewareIP, tc.depth)
			}

			// And the fingerprint folded from each side must match too,
			// since identical (UA, IP) inputs must yield identical hashes.
			const ua = "Mozilla/5.0 (regression-probe)"
			if got, want := session.Fingerprint(ua, handlerIP), session.Fingerprint(ua, middlewareIP); got != want {
				t.Fatalf("fingerprint mismatch for depth %d: login=%q request=%q", tc.depth, got, want)
			}
		})
	}
}

// TestWithTrustedProxyDepthClampsNegative documents that a negative
// depth is clamped to 0 (RemoteAddr-only) rather than wrapping into a
// huge X-Forwarded-For index, keeping the handler's resolution aligned
// with the middleware default when misconfigured.
func TestWithTrustedProxyDepthClampsNegative(t *testing.T) {
	h := (&Handler{}).WithTrustedProxyDepth(-5)
	if h.trustedProxyDepth != 0 {
		t.Fatalf("negative depth not clamped: got %d, want 0", h.trustedProxyDepth)
	}
}
