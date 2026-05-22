package middleware

import (
	"net/http"
	"strings"
)

// SecurityHeadersOptions controls the values emitted by
// SecurityHeaders. Every field has a safe production default
// derived from Load(), so callers can pass an empty struct in
// tests; production callers should pass values populated from
// the server config so operators can roll out CSP changes
// without a recompile.
type SecurityHeadersOptions struct {
	// CSPReportOnly emits the policy under
	// Content-Security-Policy-Report-Only instead of
	// Content-Security-Policy. Browsers report violations but
	// do not block — the safe rollout mode when introducing a
	// new policy on an existing site. Switch to enforcing
	// (false) once the report stream is clean.
	CSPReportOnly bool

	// CSPReportURI is appended as `report-uri <value>` to the
	// CSP value when non-empty. The browser POSTs JSON
	// violation reports to that URI so operators can see what
	// would have been blocked before flipping CSPReportOnly to
	// false. The newer `report-to` directive is intentionally
	// NOT emitted (it requires a separate Report-To header
	// chain and the Reporting API endpoint, neither of which
	// is wired yet — `report-uri` still works in every browser
	// even though it's marked legacy).
	CSPReportURI string

	// CSPConnectExtra is a list of additional origins to allow
	// in `connect-src`, on top of `'self' wss: ws:`. The
	// frontend issues XHR / fetch / WebSocket requests to (a)
	// the API on the same origin and (b) the zk-object-fabric
	// gateway for direct-to-storage uploads / downloads. The
	// gateway origin is deployment-specific (e.g.
	// `https://fabric-gw.example.com`) and must be added here
	// — otherwise presigned URLs fail with a "Refused to
	// connect" console error in production.
	CSPConnectExtra []string

	// CSPImgExtra is a list of additional origins to allow in
	// `img-src`, on top of `'self' data: blob:`. Frontend
	// thumbnails and preview images may be served from the
	// fabric storage gateway via presigned URLs; the gateway
	// origin goes here.
	CSPImgExtra []string

	// DisableHSTS skips emitting Strict-Transport-Security.
	// Used for local HTTP development (where HSTS would lock a
	// developer's browser into requiring HTTPS for localhost
	// for a year) and for environments behind a TLS-terminating
	// proxy that already emits its own HSTS header.
	DisableHSTS bool
}

// SecurityHeaders returns a middleware that emits the modern
// browser security header set on every response. Headers are
// written via w.Header().Set() BEFORE the inner handler runs so
// they're guaranteed to be in the response even when the handler
// (a) writes a body immediately, (b) calls WriteHeader and then
// returns, or (c) panics — Go's net/http flushes the header map
// regardless. Set on every status code (200, 4xx, 5xx, 304),
// not just successful responses, so a forced-error page can't be
// the missing-header attack vector.
//
// Header inventory and rationale:
//
//   - Content-Security-Policy (or -Report-Only)
//     Blocks XSS by restricting script / style / img / connect
//     to the same origin (plus configured allow-lists). The
//     primary defense against a stolen-token attack via XSS in
//     a third-party dependency. The policy uses `'self'` for
//     scripts (NO `'unsafe-inline'`, NO `'unsafe-eval'`); Vite
//     emits external bundles so this is feasible. Style
//     retains `'unsafe-inline'` because React's `style={}` prop
//     translates to attribute styles which CSP3 governs via
//     style-src-attr (defaulting to inheriting style-src);
//     refactoring every component to CSS classes is a separate
//     workstream.
//
//   - Strict-Transport-Security (skipped when DisableHSTS)
//     max-age=31536000 (1 year), includeSubDomains, preload.
//     One-year max-age is the threshold for the Chromium HSTS
//     preload list (hstspreload.org); the includeSubDomains
//     directive prevents an attacker from MITM-ing a
//     subdomain (e.g. api.example.com vs. example.com).
//
//   - X-Content-Type-Options: nosniff
//     Stops MIME-sniffing-based XSS where the browser
//     re-interprets a JSON response as HTML if the
//     Content-Type is missing or generic.
//
//   - X-Frame-Options: DENY
//     Defense-in-depth on top of CSP `frame-ancestors 'none'`
//     for older browsers that don't honor CSP3's
//     frame-ancestors directive (IE / Edge Legacy).
//     Prevents clickjacking via iframe embedding.
//
//   - Referrer-Policy: strict-origin-when-cross-origin
//     The browser default since 2020, but pinning makes it
//     explicit. Same-origin requests carry the full URL;
//     cross-origin requests carry only the origin (so a
//     leaked Referer doesn't reveal /drive/<workspace>/<file>
//     paths to third-party sites the user navigates to).
//
//   - Permissions-Policy
//     Denies camera, microphone, geolocation, USB, payment,
//     fullscreen, autoplay, gyroscope, accelerometer, etc. —
//     features the drive app doesn't use. If a future
//     dependency tries to call `navigator.mediaDevices`, the
//     browser blocks it. Smaller attack surface for malicious
//     scripts that bypass CSP via a 0-day.
//
//   - Cross-Origin-Opener-Policy: same-origin
//     Isolates the browsing context group so a window.open'd
//     popup or window.opener-linked page can't read
//     window.opener of the drive app, blocking
//     cross-origin-isolation-based side channels (Spectre).
//
//   - Cross-Origin-Resource-Policy: same-origin
//     Stops cross-origin pages from embedding our API
//     responses as <img>/<script>/<link>/<object>. The drive
//     API never wants to be embedded by another origin (the
//     SPA is the only legitimate consumer).
func SecurityHeaders(opts SecurityHeadersOptions) func(http.Handler) http.Handler {
	csp := buildCSP(opts)
	cspHeader := "Content-Security-Policy"
	if opts.CSPReportOnly {
		cspHeader = "Content-Security-Policy-Report-Only"
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set(cspHeader, csp)
			if !opts.DisableHSTS {
				// 31536000 = 365 days. preload signals
				// inclusion intent for hstspreload.org;
				// harmless even if not actually
				// preloaded.
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
			}
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", defaultPermissionsPolicy)
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")

			next.ServeHTTP(w, r)
		})
	}
}

// defaultPermissionsPolicy denies every browser capability the
// drive app does not use. Each directive's empty parenthesised
// list `()` means "no origin is allowed" — the strictest form.
// Listed alphabetically so a future diff is reviewable. New
// directives the browser adds are denied by default in the
// current spec, but we still enumerate the long-tail of common
// ones for defense-in-depth against UA-specific extensions.
const defaultPermissionsPolicy = "accelerometer=(), " +
	"ambient-light-sensor=(), " +
	"autoplay=(), " +
	"battery=(), " +
	"camera=(), " +
	"display-capture=(), " +
	"document-domain=(), " +
	"encrypted-media=(), " +
	"fullscreen=(self), " +
	"geolocation=(), " +
	"gyroscope=(), " +
	"hid=(), " +
	"idle-detection=(), " +
	"magnetometer=(), " +
	"microphone=(), " +
	"midi=(), " +
	"payment=(), " +
	"picture-in-picture=(), " +
	"publickey-credentials-get=(), " +
	"screen-wake-lock=(), " +
	"serial=(), " +
	"sync-xhr=(), " +
	"usb=(), " +
	"web-share=(), " +
	"xr-spatial-tracking=()"

// buildCSP assembles the Content-Security-Policy value. The
// directives are emitted in stable order so the header value
// hash is reproducible across boots (useful for some CDN cache
// keys and the hash-based PWA precache).
func buildCSP(opts SecurityHeadersOptions) string {
	connect := []string{"'self'", "wss:", "ws:"}
	connect = append(connect, opts.CSPConnectExtra...)

	img := []string{"'self'", "data:", "blob:"}
	img = append(img, opts.CSPImgExtra...)

	directives := []string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src " + strings.Join(img, " "),
		"font-src 'self' data:",
		"connect-src " + strings.Join(connect, " "),
		"frame-ancestors 'none'",
		"form-action 'self'",
		"base-uri 'self'",
		"object-src 'none'",
		// upgrade-insecure-requests: a browser-visible signal
		// to rewrite any accidental http:// resource link in
		// our HTML to https://, providing belt-and-suspenders
		// on top of HSTS for first-load requests.
		"upgrade-insecure-requests",
	}
	if strings.TrimSpace(opts.CSPReportURI) != "" {
		directives = append(directives, "report-uri "+strings.TrimSpace(opts.CSPReportURI))
	}
	return strings.Join(directives, "; ")
}
