package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/zk-drive/api/middleware"
)

// writeIndex drops a minimal index.html carrying the nonce placeholder
// into a temp dir and returns the dir.
func writeIndex(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	return dir
}

// newSPARouter wires SecurityHeaders(Nonce) + spaHandler exactly as
// run() does, so the test exercises the real middleware→context→
// handler path (and pins that chi runs r.Use middleware before the
// NotFound handler — the property the nonce injection relies on).
func newSPARouter(dir string, nonce bool) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.SecurityHeaders(middleware.SecurityHeadersOptions{Nonce: nonce}))
	r.Get("/api/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.NotFound(spaHandler(dir))
	return r
}

// TestSPAHandlerInjectsMatchingNonce pins the 6.5 end-to-end contract:
// the nonce stamped into the served index.html meta tag is the SAME
// value the SecurityHeaders middleware put in the CSP script-src for
// that request. This proves chi runs the middleware before the
// NotFound SPA handler and that the context plumbing is intact.
func TestSPAHandlerInjectsMatchingNonce(t *testing.T) {
	dir := writeIndex(t, `<!doctype html><html><head><meta name="csp-nonce" content="__CSP_NONCE__" /></head><body><div id="root"></div></body></html>`)
	srv := newSPARouter(dir, true)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/drive/some/deep/route", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "__CSP_NONCE__") {
		t.Fatalf("placeholder not replaced in served HTML: %s", body)
	}

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
	nonce := rest[:j]
	if nonce == "" {
		t.Fatalf("empty nonce in CSP: %s", csp)
	}
	if !strings.Contains(body, `content="`+nonce+`"`) {
		t.Errorf("served HTML nonce does not match CSP nonce %q; body: %s", nonce, body)
	}
	// index.html must never be cached (it points at hashed bundles).
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") && !strings.Contains(cc, "no-store") {
		t.Errorf("index.html Cache-Control = %q, want no-store/no-cache", cc)
	}
}

// TestSPAHandlerNonceDisabledClearsPlaceholder pins that when nonces
// are disabled the placeholder is replaced with an empty string (never
// left as the literal __CSP_NONCE__ token in the shipped HTML).
func TestSPAHandlerNonceDisabledClearsPlaceholder(t *testing.T) {
	dir := writeIndex(t, `<meta name="csp-nonce" content="__CSP_NONCE__" />`)
	srv := newSPARouter(dir, false)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	body := rec.Body.String()
	if strings.Contains(body, "__CSP_NONCE__") {
		t.Errorf("placeholder leaked into HTML when nonces disabled: %s", body)
	}
	if !strings.Contains(body, `content=""`) {
		t.Errorf("expected empty nonce content when disabled; body: %s", body)
	}
}

// TestSPAHandlerInjectsNonceAtEveryPlaceholder pins that the handler
// substitutes EVERY __CSP_NONCE__ occurrence, not just the first — so a
// future index.html that also stamps the nonce onto an inline
// <script nonce="__CSP_NONCE__"> (the natural next use of the nonce)
// gets a working script-src match instead of a literal token that the
// browser would reject. Guards the segment-split in loadIndexTemplate.
func TestSPAHandlerInjectsNonceAtEveryPlaceholder(t *testing.T) {
	dir := writeIndex(t, `<meta name="csp-nonce" content="__CSP_NONCE__" /><script nonce="__CSP_NONCE__">/*inline*/</script>`)
	srv := newSPARouter(dir, true)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/drive", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "__CSP_NONCE__") {
		t.Fatalf("a placeholder occurrence was left unreplaced: %s", body)
	}

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
	nonce := rest[:j]
	// Both the meta tag and the inline <script> must carry the live nonce.
	if !strings.Contains(body, `content="`+nonce+`"`) {
		t.Errorf("meta nonce not substituted with %q; body: %s", nonce, body)
	}
	if !strings.Contains(body, `<script nonce="`+nonce+`">`) {
		t.Errorf("inline script nonce not substituted with %q; body: %s", nonce, body)
	}
	if got := strings.Count(body, nonce); got != 2 {
		t.Errorf("expected nonce substituted at 2 placeholders, found %d; body: %s", got, body)
	}
}

// TestSPAHandlerServesAssetsVerbatim pins that concrete asset files are
// streamed as-is (not templated) — only the HTML document is rewritten.
func TestSPAHandlerServesAssetsVerbatim(t *testing.T) {
	dir := writeIndex(t, `<meta name="csp-nonce" content="__CSP_NONCE__" />`)
	const js = "console.log('__CSP_NONCE__ stays literal in JS');"
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(js), 0o600); err != nil {
		t.Fatalf("write app.js: %v", err)
	}
	srv := newSPARouter(dir, true)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))

	if got := rec.Body.String(); got != js {
		t.Errorf("asset body mutated: got %q want %q", got, js)
	}
}
