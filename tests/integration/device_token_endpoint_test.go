package integration

import (
	"net/http"
	"testing"
)

// TestRegisterDeviceEndpoint drives the POST/DELETE /api/push/register-device
// HTTP path end-to-end: a valid registration is accepted, malformed
// input is rejected, an unknown platform is a 400, and unregister is
// idempotent.
func TestRegisterDeviceEndpoint(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("PushCo", "admin@push.test", "Pat", "password-pp")

	// Valid iOS registration → 204.
	status, body := env.httpRequest(http.MethodPost, "/api/push/register-device", tok.Token, map[string]string{
		"platform": "ios",
		"token":    "apns-device-token-abc",
	})
	if status != http.StatusNoContent {
		t.Fatalf("register ios: status=%d body=%s", status, string(body))
	}

	// Re-registering the same token is an idempotent 204 (upsert).
	status, _ = env.httpRequest(http.MethodPost, "/api/push/register-device", tok.Token, map[string]string{
		"platform": "ios",
		"token":    "apns-device-token-abc",
	})
	if status != http.StatusNoContent {
		t.Fatalf("re-register ios: status=%d", status)
	}

	// Unknown platform → 400 validation error.
	status, _ = env.httpRequest(http.MethodPost, "/api/push/register-device", tok.Token, map[string]string{
		"platform": "blackberry",
		"token":    "x",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown platform: status=%d, want 400", status)
	}

	// Missing token → 400.
	status, _ = env.httpRequest(http.MethodPost, "/api/push/register-device", tok.Token, map[string]string{
		"platform": "android",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("missing token: status=%d, want 400", status)
	}

	// Unregister the token → 204, and again → 204 (idempotent).
	for i := 0; i < 2; i++ {
		status, _ = env.httpRequest(http.MethodDelete, "/api/push/register-device", tok.Token, map[string]string{
			"platform": "ios",
			"token":    "apns-device-token-abc",
		})
		if status != http.StatusNoContent {
			t.Fatalf("unregister attempt %d: status=%d, want 204", i, status)
		}
	}

	// Unauthenticated request → 401 (no token).
	status, _ = env.httpRequest(http.MethodPost, "/api/push/register-device", "", map[string]string{
		"platform": "ios",
		"token":    "y",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d, want 401", status)
	}
}
