package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/kennguy3n/zk-drive/internal/billing"
	"github.com/kennguy3n/zk-drive/internal/notification"
)

// TestPhase5Gate exercises every Phase 5 deliverable end-to-end so the
// closing decision gate ("a paying customer can sign up via Stripe
// Checkout, install the PWA on mobile, receive real-time WebSocket
// notifications, and see PDF previews") is pinned in CI.
//
// The subtests are intentionally light on assertions about external
// systems — Stripe API calls, an LLM provider, and a real Redis
// instance are not in scope for this harness. What we care about is
// that the *wiring* is in place: the routes exist, return non-404
// status codes, and the fall-back paths (no-secret, no-Redis) behave
// the way the production server expects.
func TestPhase5Gate(t *testing.T) {
	env := setupEnv(t)
	admin := env.signupAndLogin("Acme Phase 5", "admin@phase5.test", "Alice", "pw")

	t.Run("StripeWebhookEndpointExists", func(t *testing.T) {
		// The harness wires StripeService with an empty webhook
		// secret, so the handler returns 400 before signature
		// verification. The point of this subtest is that the
		// route is mounted at all — i.e. *not* 404.
		status, body := env.httpRequest(http.MethodPost, "/api/webhooks/stripe", "", map[string]any{
			"id":   "evt_test_phase5",
			"type": "checkout.session.completed",
		})
		if status == http.StatusNotFound {
			t.Fatalf("stripe webhook route missing: status=%d body=%s", status, string(body))
		}
		// Configured deployments return 200 for handled events;
		// the unconfigured / bad-signature path returns 400.
		if status != http.StatusOK && status != http.StatusBadRequest {
			t.Fatalf("stripe webhook unexpected status=%d body=%s", status, string(body))
		}
	})

	t.Run("BillingCheckoutSessionEndpointExists", func(t *testing.T) {
		// Without STRIPE_SECRET_KEY the handler returns 501 Not
		// Implemented; with bad input it returns 400. Either is
		// fine — what matters for the gate is that the route is
		// mounted (i.e. *not* 404).
		status, body := env.httpRequest(http.MethodPost, "/api/admin/billing/checkout-session", admin.Token,
			map[string]any{
				"tier":        billing.TierBusiness,
				"success_url": "https://drive.example.test/billing/success",
				"cancel_url":  "https://drive.example.test/billing/cancel",
			})
		if status == http.StatusNotFound {
			t.Fatalf("checkout-session route missing: status=%d body=%s", status, string(body))
		}
		if status < 400 {
			t.Fatalf("checkout-session unexpected success without stripe key: status=%d body=%s", status, string(body))
		}
	})

	t.Run("BillingPortalSessionEndpointExists", func(t *testing.T) {
		status, body := env.httpRequest(http.MethodPost, "/api/admin/billing/portal-session", admin.Token,
			map[string]any{
				"return_url": "https://drive.example.test/billing",
			})
		if status == http.StatusNotFound {
			t.Fatalf("portal-session route missing: status=%d body=%s", status, string(body))
		}
		if status < 400 {
			t.Fatalf("portal-session unexpected success without stripe customer: status=%d body=%s", status, string(body))
		}
	})

	t.Run("PWAManifest", func(t *testing.T) {
		// vite-plugin-pwa generates frontend/dist/manifest.webmanifest
		// during `npm run build`. The integration job in CI does
		// not build the frontend, so the file may be missing on
		// CI runners — skip in that case rather than fail.
		manifestPath := findFrontendDistFile(t, "manifest.webmanifest")
		if manifestPath == "" {
			t.Skip("frontend/dist/manifest.webmanifest not present; run `npm run build` to populate it")
		}
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatalf("read manifest %s: %v", manifestPath, err)
		}
		var manifest struct {
			Name      string `json:"name"`
			ShortName string `json:"short_name"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatalf("parse manifest: %v (raw=%s)", err, string(raw))
		}
		if manifest.Name != "ZK Drive" {
			t.Errorf("manifest name = %q, want %q", manifest.Name, "ZK Drive")
		}
	})

	t.Run("FrontendCodeSplitting", func(t *testing.T) {
		// Build-time check: vite + React.lazy() should emit at
		// least two .js chunks under dist/assets/. CI without a
		// frontend build skips.
		assetsDir := findFrontendDistDir(t, "assets")
		if assetsDir == "" {
			t.Skip("frontend/dist/assets not present; run `npm run build` to populate it")
		}
		matches, err := filepath.Glob(filepath.Join(assetsDir, "*.js"))
		if err != nil {
			t.Fatalf("glob js chunks: %v", err)
		}
		if len(matches) < 2 {
			t.Errorf("expected >= 2 js chunks (code splitting), got %d: %v", len(matches), matches)
		}
	})

	t.Run("WebSocketUpgrade", func(t *testing.T) {
		// Dial /api/ws as the admin and verify the upgrade
		// handshake succeeds (101 Switching Protocols).
		wsURL := "ws" + strings.TrimPrefix(env.server.URL, "http") + "/api/ws"
		header := http.Header{}
		header.Set("Authorization", "Bearer "+admin.Token)
		dialer := &gws.Dialer{HandshakeTimeout: 5 * time.Second}
		conn, resp, err := dialer.Dial(wsURL, header)
		if err != nil {
			if resp != nil {
				t.Fatalf("dial %s: %v (status=%d)", wsURL, err, resp.StatusCode)
			}
			t.Fatalf("dial %s: %v", wsURL, err)
		}
		defer conn.Close()
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("expected 101 Switching Protocols, got %d", resp.StatusCode)
		}
	})

	t.Run("RedisRateLimiting", func(t *testing.T) {
		if os.Getenv("REDIS_URL") == "" {
			t.Skip("REDIS_URL not configured; Redis rate-limit subtest skipped")
		}
		// When REDIS_URL is set in CI, this subtest fires a burst
		// of /api/folders requests and asserts the limiter
		// eventually returns 429. The harness uses a permissive
		// in-memory limiter (PerUser=0), so this subtest is only
		// meaningful in deployments that override the wiring with
		// a real Redis-backed limiter.
		t.Skip("REDIS_URL set, but harness uses permissive in-memory limiter; verified at unit level in middleware/ratelimit_redis_test.go")
	})

	t.Run("AISummaryWithLLMFallback", func(t *testing.T) {
		// Create a managed-encrypted KChat room (default mode) and
		// drop a single file in so the scaffold has something to
		// summarise. The summary endpoint returns the LLM output
		// when LLM_API_KEY is set, and the rule-based scaffold
		// otherwise — both shapes are non-empty strings.
		const roomID = "kchat-room-phase5-summary"
		status, body := env.httpRequest(http.MethodPost, "/api/kchat/rooms", admin.Token, map[string]string{
			"kchat_room_id": roomID,
		})
		if status != http.StatusCreated {
			t.Fatalf("create kchat room: status=%d body=%s", status, string(body))
		}
		var room kchatRoomCreated
		env.decodeJSON(body, &room)

		createFile(t, env, admin.Token, room.FolderID.String(), "phase5-notes.txt", "text/plain")

		status, body = env.httpRequest(http.MethodPost, "/api/kchat/rooms/"+room.ID.String()+"/summary", admin.Token, nil)
		if status != http.StatusOK {
			t.Fatalf("summary: status=%d body=%s", status, string(body))
		}
		var resp struct {
			Summary string `json:"summary"`
		}
		env.decodeJSON(body, &resp)
		if strings.TrimSpace(resp.Summary) == "" {
			t.Fatalf("expected non-empty summary, got %q", resp.Summary)
		}
	})

	t.Run("PDFPreviewSupport", func(t *testing.T) {
		// Heavy end-to-end PDF preview generation requires a real
		// storage backend (presigned PUT/GET) so the worker can
		// fetch source bytes and upload the rendered thumbnail.
		// The harness stubs S3 to a non-routable endpoint, so we
		// only assert the prerequisite tooling is present and let
		// the dedicated unit test (preview/pdf_test.go's
		// TestPDFPreviewGeneration) cover the rasterisation path.
		if _, err := exec.LookPath("pdftoppm"); err != nil {
			t.Skip("pdftoppm not available; PDF preview path skipped")
		}
		// Also exercise the metadata round-trip: create a PDF
		// file row and verify the file metadata API returns the
		// supported mime type so the preview worker would pick it
		// up.
		fold := createFolder(t, env, admin.Token, nil, "Phase5 PDFs")
		f := createFile(t, env, admin.Token, fold.ID.String(), "phase5.pdf", "application/pdf")
		status, body := env.httpRequest(http.MethodGet, "/api/files/"+f.ID.String(), admin.Token, nil)
		if status != http.StatusOK {
			t.Fatalf("get pdf metadata: status=%d body=%s", status, string(body))
		}
	})

	t.Run("PreviousPhaseGatesStillPass", func(t *testing.T) {
		// Billing usage summary (Phase 3 quota infrastructure).
		status, body := env.httpRequest(http.MethodGet, "/api/admin/billing/usage", admin.Token, nil)
		if status != http.StatusOK {
			t.Fatalf("billing usage: status=%d body=%s", status, string(body))
		}
		var usage billing.UsageSummary
		env.decodeJSON(body, &usage)
		if usage.Tier == "" {
			t.Errorf("billing usage returned empty tier: %+v", usage)
		}

		// Notifications listing (Phase 3 fan-out).
		status, body = env.httpRequest(http.MethodGet, "/api/notifications", admin.Token, nil)
		if status != http.StatusOK {
			t.Fatalf("notifications: status=%d body=%s", status, string(body))
		}
		var notifResp struct {
			Notifications []notification.Notification `json:"notifications"`
		}
		env.decodeJSON(body, &notifResp)

		// Search endpoint (Phase 4 strict-ZK / FTS).
		status, body = env.httpRequest(http.MethodGet, "/api/search?q=phase5", admin.Token, nil)
		if status != http.StatusOK {
			t.Fatalf("search: status=%d body=%s", status, string(body))
		}
	})
}

// findFrontendDistFile walks up from the test file looking for
// frontend/dist/<name>. Returns "" when the file is not present so
// callers can decide whether to skip or fail.
func findFrontendDistFile(t *testing.T, name string) string {
	t.Helper()
	root := findRepoRoot(t)
	if root == "" {
		return ""
	}
	candidate := filepath.Join(root, "frontend", "dist", name)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return ""
}

// findFrontendDistDir is the directory equivalent of
// findFrontendDistFile; it returns the resolved path to
// frontend/dist/<name> when the directory exists, or "" otherwise.
func findFrontendDistDir(t *testing.T, name string) string {
	t.Helper()
	root := findRepoRoot(t)
	if root == "" {
		return ""
	}
	candidate := filepath.Join(root, "frontend", "dist", name)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return ""
}

// findRepoRoot walks upwards from this test file looking for go.mod,
// which marks the zk-drive repo root. Mirrors findMigrationsDir in
// setup_test.go but scoped to dist artefacts so the helper is
// reusable from later phase gate tests.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(file)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return ""
}
