package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSecurityHeadersDefaultsAllPresent pins the contract that
// every header in the default set is emitted with the exact
// value our threat model requires. A regression that silently
// drops a header (e.g. an upstream library that overrides
// w.Header() mid-handler) would fail this test.
func TestSecurityHeadersDefaultsAllPresent(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	cases := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "strict-origin-when-cross-origin",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Strict-Transport-Security":    "max-age=31536000; includeSubDomains; preload",
	}
	for k, want := range cases {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if perm := rec.Header().Get("Permissions-Policy"); !strings.Contains(perm, "camera=()") || !strings.Contains(perm, "microphone=()") || !strings.Contains(perm, "geolocation=()") {
		t.Errorf("Permissions-Policy missing critical denies: %s", perm)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Errorf("Content-Security-Policy not set in enforce mode")
	}
	if rec.Header().Get("Content-Security-Policy-Report-Only") != "" {
		t.Errorf("Content-Security-Policy-Report-Only set when CSPReportOnly=false")
	}
}

// TestSecurityHeadersCSPEnumeratesExpectedDirectives pins every
// individual directive in the default CSP. A drift to a more
// permissive policy (e.g. someone adds `'unsafe-inline'` to
// script-src to unblock a debugging session and forgets to
// remove it) would fail this test.
func TestSecurityHeadersCSPEnumeratesExpectedDirectives(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	required := []string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"frame-ancestors 'none'",
		"form-action 'self'",
		"base-uri 'self'",
		"object-src 'none'",
		"upgrade-insecure-requests",
	}
	for _, d := range required {
		if !strings.Contains(csp, d) {
			t.Errorf("CSP missing directive %q; full header: %s", d, csp)
		}
	}
	// Negative: 'unsafe-eval' MUST NOT appear in script-src.
	// JavaScript that needs eval() in 2026 is malware.
	if strings.Contains(csp, "'unsafe-eval'") {
		t.Errorf("CSP contains 'unsafe-eval' (forbidden): %s", csp)
	}
	// Negative: script-src MUST NOT have 'unsafe-inline'.
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP script-src has 'unsafe-inline' (forbidden): %s", csp)
	}
}

// TestSecurityHeadersReportOnlyModeUsesAlternateHeader pins the
// rollout-mode contract: when CSPReportOnly is true the policy
// emits under the Content-Security-Policy-Report-Only header
// instead of Content-Security-Policy, so browsers report
// violations without blocking. Operators flip this to false
// once the report stream is clean.
func TestSecurityHeadersReportOnlyModeUsesAlternateHeader(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{CSPReportOnly: true})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Errorf("Content-Security-Policy set in report-only mode")
	}
	if rec.Header().Get("Content-Security-Policy-Report-Only") == "" {
		t.Errorf("Content-Security-Policy-Report-Only missing in report-only mode")
	}
}

// TestSecurityHeadersCSPReportURIAppended pins that the
// configured CSP_REPORT_URI is appended to the CSP value as a
// `report-uri` directive, so browser violation reports POST to
// the operator's collector (e.g. /api/csp/report or Sentry).
func TestSecurityHeadersCSPReportURIAppended(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{
		CSPReportURI: "/api/csp/report",
	})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "report-uri /api/csp/report") {
		t.Errorf("CSP missing report-uri directive; full header: %s", csp)
	}
}

// TestSecurityHeadersCSPConnectAndImgExtraMergedIn pins that
// CSPConnectExtra and CSPImgExtra origins are merged into the
// emitted policy. Production deployments need this to allow
// presigned URLs to the fabric storage gateway origin.
func TestSecurityHeadersCSPConnectAndImgExtraMergedIn(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{
		CSPConnectExtra: []string{"https://fabric-gw.example.com", "https://api.stripe.com"},
		CSPImgExtra:     []string{"https://fabric-gw.example.com"},
	})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' https://fabric-gw.example.com https://api.stripe.com") {
		t.Errorf("CSP connect-src missing extras; full header: %s", csp)
	}
	// Defense-in-depth: the default connect-src must NOT carry
	// bare wss: / ws: scheme sources — those would let an XSS
	// payload exfiltrate via WebSocket to any host. 'self'
	// covers same-origin WS upgrades in modern browsers, and
	// cross-origin WS gateways must opt in via CSPConnectExtra.
	for _, scheme := range []string{" wss:", " ws:"} {
		if strings.Contains(csp, scheme) {
			t.Errorf("CSP connect-src contains bare %q scheme source (XSS exfil vector); full header: %s", scheme, csp)
		}
	}
	if !strings.Contains(csp, "img-src 'self' data: blob: https://fabric-gw.example.com") {
		t.Errorf("CSP img-src missing extras; full header: %s", csp)
	}
}

// TestSecurityHeadersDisableHSTSOmitsHSTSKeepsRest pins that
// the DisableHSTS knob suppresses Strict-Transport-Security
// AND the CSP `upgrade-insecure-requests` directive — both
// would break local plain-HTTP development (HSTS locks the
// browser into HTTPS-only for a year; upgrade-insecure-requests
// on a "potentially trustworthy" localhost origin is
// implementation-defined and could rewrite all fetches to
// https://). Every other header still emits.
func TestSecurityHeadersDisableHSTSOmitsHSTSKeepsRest(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{DisableHSTS: true})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Errorf("HSTS set despite DisableHSTS=true")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options not set when DisableHSTS=true")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Errorf("CSP not set when DisableHSTS=true")
	}
	if strings.Contains(csp, "upgrade-insecure-requests") {
		t.Errorf("CSP contains upgrade-insecure-requests despite DisableHSTS=true (would break local plain-HTTP); got %s", csp)
	}
}

// TestSecurityHeadersUpgradeInsecureRequestsEmittedWhenHSTSEnabled
// pins the inverse: when HSTS is enabled (production default),
// the CSP carries `upgrade-insecure-requests` so a stale
// http:// link in our markup gets transparently upgraded by
// the browser. This is the belt-and-suspenders companion to
// HSTS on the first-load request.
func TestSecurityHeadersUpgradeInsecureRequestsEmittedWhenHSTSEnabled(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "upgrade-insecure-requests") {
		t.Errorf("CSP missing upgrade-insecure-requests in default (HSTS-enabled) mode; got %s", csp)
	}
}

// TestSecurityHeadersCSPExtrasRejectInjection pins that an
// operator who fat-fingers an env var with a semicolon, comma,
// or whitespace doesn't get CSP-directive injection. A value
// like `https://gw.example.com; script-src 'unsafe-inline'`
// would otherwise silently flip script-src to unsafe-inline.
// Offending entries are dropped (no escape mechanism in the
// CSP grammar; the browser console error from the resulting
// blocked-resource is the signal that the env var is wrong).
func TestSecurityHeadersCSPExtrasRejectInjection(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{
		CSPConnectExtra: []string{
			"https://gw.example.com",                          // legit
			"https://evil.example.com; script-src 'unsafe-inline'", // semicolon injection
			"https://comma.example.com, default-src *",        // comma injection
			"https://whitespace example.com",                  // whitespace injection
			"  https://trimmed.example.com  ",                 // legit after trim
		},
		CSPImgExtra: []string{
			"https://img.example.com",
			"https://bad.example.com\n; img-src *",            // newline injection
		},
		CSPReportURI: "https://csp.example.com/report; script-src 'unsafe-inline'",
	})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	// connect-src kept legit values, dropped injectors.
	if !strings.Contains(csp, "https://gw.example.com") {
		t.Errorf("CSP dropped legit connect origin; got %s", csp)
	}
	if !strings.Contains(csp, "https://trimmed.example.com") {
		t.Errorf("CSP dropped legit trimmed connect origin; got %s", csp)
	}
	// img-src kept legit, dropped injector.
	if !strings.Contains(csp, "https://img.example.com") {
		t.Errorf("CSP dropped legit img origin; got %s", csp)
	}
	// CRITICAL: none of the injection attempts landed.
	for _, bad := range []string{
		"https://evil.example.com",
		"script-src 'unsafe-inline'",
		"default-src *",
		"img-src *",
		"https://comma.example.com",
		"https://whitespace example.com",
		"https://bad.example.com",
		// The report-uri value also contained an injection;
		// it must be suppressed entirely.
		"https://csp.example.com/report",
	} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP injection landed: %q present in policy %s", bad, csp)
		}
	}
}

// TestSecurityHeadersAppliedOnEveryStatusCode pins that the
// headers are emitted on 4xx and 5xx responses too, not just
// 2xx. A status-conditional miss would let an attacker probe
// 404 pages to find the unhardened response surface.
func TestSecurityHeadersAppliedOnEveryStatusCode(t *testing.T) {
	codes := []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError}
	for _, code := range codes {
		t.Run(http.StatusText(code), func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			})
			wrapped := SecurityHeaders(SecurityHeadersOptions{})(handler)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			wrapped.ServeHTTP(rec, req)

			if rec.Code != code {
				t.Fatalf("status = %d, want %d", rec.Code, code)
			}
			if rec.Header().Get("Content-Security-Policy") == "" {
				t.Errorf("CSP missing on %d response", code)
			}
			if rec.Header().Get("X-Frame-Options") != "DENY" {
				t.Errorf("X-Frame-Options missing on %d response", code)
			}
		})
	}
}

// TestSecurityHeadersAppliedOnAllMethods pins that the headers
// are emitted on POST / PUT / DELETE / OPTIONS too. A
// method-conditional miss would let a write-side attack on the
// API land without CSP.
func TestSecurityHeadersAppliedOnAllMethods(t *testing.T) {
	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			wrapped := SecurityHeaders(SecurityHeadersOptions{})(passthroughHandler())

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(m, "/x", nil)
			wrapped.ServeHTTP(rec, req)

			if rec.Header().Get("Content-Security-Policy") == "" {
				t.Errorf("CSP missing on %s response", m)
			}
		})
	}
}

// TestSecurityHeadersWrittenBeforeHandlerRunsSoEarlyWriteHeaderWorks
// pins that the headers are set BEFORE the inner handler is
// invoked. A handler that calls WriteHeader(200) and then
// returns must still emit all security headers in the response —
// otherwise an early-write handler (rate-limit reject, body
// limit reject, panic-recovery 500) would skip the hardening.
func TestSecurityHeadersWrittenBeforeHandlerRunsSoEarlyWriteHeaderWorks(t *testing.T) {
	earlyWrite := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	wrapped := SecurityHeaders(SecurityHeadersOptions{})(earlyWrite)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("CSP missing on early-write 429 response — headers must be set before WriteHeader commits them")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options missing on early-write 429 response")
	}
}

// TestSecurityHeadersDoNotOverrideHandlerSetHeaders pins the
// contract that if a handler explicitly sets one of the headers
// (e.g. a CSP page-specific override for a /share/<token> guest
// page that needs to embed a preview from the storage gateway),
// the handler's value wins. Use w.Header().Set(...) AFTER
// next.ServeHTTP — but in practice handlers run AFTER our
// middleware writes its defaults, so handler Set() naturally
// overrides ours. This test pins that ordering.
func TestSecurityHeadersDoNotOverrideHandlerSetHeaders(t *testing.T) {
	override := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.WriteHeader(http.StatusOK)
	})
	wrapped := SecurityHeaders(SecurityHeadersOptions{})(override)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("handler override lost: X-Frame-Options = %q, want SAMEORIGIN", got)
	}
}

// TestSecurityHeadersNonceAddedToScriptSrcAndContext pins the 6.5
// nonce contract: when Nonce is enabled the CSP script-src carries a
// `'nonce-<base64>'` source AND the same nonce is exposed on the
// request context for the SPA handler to echo into the index.html
// meta tag. The nonce must be the SAME value in both places, must keep
// `'self'` (so external bundles still load), and must NOT reintroduce
// `'unsafe-inline'`.
func TestSecurityHeadersNonceAddedToScriptSrcAndContext(t *testing.T) {
	var ctxNonce string
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxNonce = CSPNonceFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	wrapped := SecurityHeaders(SecurityHeadersOptions{Nonce: true})(capture)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	if ctxNonce == "" {
		t.Fatalf("nonce not propagated to request context")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	wantSrc := "script-src 'self' 'nonce-" + ctxNonce + "'"
	if !strings.Contains(csp, wantSrc) {
		t.Errorf("CSP script-src missing nonce; want %q in %s", wantSrc, csp)
	}
	if strings.Contains(csp, "'unsafe-inline'") && strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("nonce mode must not reintroduce script-src 'unsafe-inline': %s", csp)
	}
	if strings.Contains(csp, cspNonceSentinel) {
		t.Errorf("CSP still contains the unrendered nonce sentinel: %s", csp)
	}
}

// TestSecurityHeadersNonceIsPerRequest pins that two requests get two
// distinct nonces — a fixed nonce would be no better than
// 'unsafe-inline' (an attacker could just reuse it).
func TestSecurityHeadersNonceIsPerRequest(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{Nonce: true})(passthroughHandler())

	nonceOf := func() string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		wrapped.ServeHTTP(rec, req)
		csp := rec.Header().Get("Content-Security-Policy")
		const marker = "'nonce-"
		i := strings.Index(csp, marker)
		if i < 0 {
			t.Fatalf("no nonce in CSP: %s", csp)
		}
		rest := csp[i+len(marker):]
		j := strings.Index(rest, "'")
		if j < 0 {
			t.Fatalf("unterminated nonce in CSP: %s", csp)
		}
		return rest[:j]
	}

	a, b := nonceOf(), nonceOf()
	if a == "" || b == "" {
		t.Fatalf("empty nonce(s): %q %q", a, b)
	}
	if a == b {
		t.Errorf("nonce reused across requests: %q", a)
	}
}

// TestSecurityHeadersNonceDisabledByDefault pins that with the
// zero-value options (Nonce:false) no nonce source appears and no
// unrendered sentinel leaks into the policy.
func TestSecurityHeadersNonceDisabledByDefault(t *testing.T) {
	wrapped := SecurityHeaders(SecurityHeadersOptions{})(passthroughHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	wrapped.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "'nonce-") {
		t.Errorf("nonce source present despite Nonce=false: %s", csp)
	}
	if strings.Contains(csp, cspNonceSentinel) {
		t.Errorf("nonce sentinel leaked into policy: %s", csp)
	}
	if CSPNonceFromContext(req.Context()) != "" {
		t.Errorf("context nonce set despite Nonce=false")
	}
}

// TestSecurityHeadersExpectCT pins the 6.5 Expect-CT contract: emitted
// (with enforce) when ExpectCT is on and HSTS is enabled, suppressed
// when HSTS is disabled (Expect-CT is HTTPS-only), and absent when the
// option is off.
func TestSecurityHeadersExpectCT(t *testing.T) {
	t.Run("emitted when enabled and HSTS on", func(t *testing.T) {
		wrapped := SecurityHeaders(SecurityHeadersOptions{ExpectCT: true})(passthroughHandler())
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if got := rec.Header().Get("Expect-CT"); got != "max-age=86400, enforce" {
			t.Errorf("Expect-CT = %q, want %q", got, "max-age=86400, enforce")
		}
	})
	t.Run("suppressed when HSTS disabled", func(t *testing.T) {
		wrapped := SecurityHeaders(SecurityHeadersOptions{ExpectCT: true, DisableHSTS: true})(passthroughHandler())
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if got := rec.Header().Get("Expect-CT"); got != "" {
			t.Errorf("Expect-CT set despite DisableHSTS: %q", got)
		}
	})
	t.Run("absent when disabled", func(t *testing.T) {
		wrapped := SecurityHeaders(SecurityHeadersOptions{})(passthroughHandler())
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if got := rec.Header().Get("Expect-CT"); got != "" {
			t.Errorf("Expect-CT set despite ExpectCT=false: %q", got)
		}
	})
}

func passthroughHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}
