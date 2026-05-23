package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// TestTOTPLifecycle exercises the WS-19 second-factor end-to-end:
// signup -> enroll/begin -> enroll/finalize -> login (returns
// challenge token) -> verify -> session-token granted. Then disables
// 2FA and re-logs in without a second factor.
//
// This is the load-bearing integration test for the feature; the
// internal/totp package has separate unit tests for the RFC 6238
// vectors, replay protection, and recovery-code semantics.
func TestTOTPLifecycle(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	// Begin enrollment from an authenticated session.
	status, body := env.httpRequest(http.MethodPost, "/api/auth/totp/enroll/begin", tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("enroll/begin: status=%d body=%s", status, string(body))
	}
	var begin struct {
		Secret       string `json:"secret"`
		OtpauthURI   string `json:"otpauth_uri"`
		QRCodePNG    string `json:"qr_code_png"`
		AlreadyBound bool   `json:"already_pending"`
	}
	env.decodeJSON(body, &begin)
	if begin.Secret == "" {
		t.Fatalf("enroll/begin: empty secret in response")
	}

	// Finalize with a freshly-generated code.
	code, err := totp.GenerateCode(begin.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	status, body = env.httpRequest(http.MethodPost, "/api/auth/totp/enroll/finalize", tok.Token, map[string]string{
		"code": code,
	})
	if status != http.StatusOK {
		t.Fatalf("enroll/finalize: status=%d body=%s", status, string(body))
	}
	var finalize struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	env.decodeJSON(body, &finalize)
	if len(finalize.RecoveryCodes) != 10 {
		t.Fatalf("finalize: expected 10 recovery codes, got %d", len(finalize.RecoveryCodes))
	}

	// Now login: should return an mfa_required response instead of
	// a session token.
	status, body = env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "correct-horse",
	})
	if status != http.StatusOK {
		t.Fatalf("login: status=%d body=%s", status, string(body))
	}
	var mfaResp struct {
		MFARequired bool   `json:"mfa_required"`
		MFAToken    string `json:"mfa_token"`
		MustEnroll  bool   `json:"must_enroll"`
	}
	env.decodeJSON(body, &mfaResp)
	if !mfaResp.MFARequired || mfaResp.MFAToken == "" {
		t.Fatalf("login after enrollment: expected mfa_required+mfa_token, got %+v body=%s", mfaResp, string(body))
	}
	if mfaResp.MustEnroll {
		t.Fatal("login: must_enroll set when user is already enrolled")
	}

	// Sleep just over a TOTP period boundary so the verify code is
	// different from the finalize code (replay protection asserts
	// last_used_at strict monotonicity).
	time.Sleep(31 * time.Second)
	code2, err := totp.GenerateCode(begin.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate verify code: %v", err)
	}

	// Verify with the challenge token.
	status, body = env.httpRequest(http.MethodPost, "/api/auth/totp/verify", mfaResp.MFAToken, map[string]string{
		"code": code2,
	})
	if status != http.StatusOK {
		t.Fatalf("verify: status=%d body=%s", status, string(body))
	}
	var session tokenPayload
	env.decodeJSON(body, &session)
	if session.Token == "" {
		t.Fatal("verify: empty session token")
	}

	// The session token must be usable against a protected route.
	status, _ = env.httpRequest(http.MethodGet, "/api/workspaces", session.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("/api/workspaces after verify: status=%d", status)
	}

	// And the challenge token must NOT be: it has purpose=mfa_challenge.
	status, _ = env.httpRequest(http.MethodGet, "/api/workspaces", mfaResp.MFAToken, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("challenge token on data plane: status=%d, want 401", status)
	}

	// Disable 2FA. Requires password re-verify. Returns 204 No Content
	// on success — the handler has no useful payload to return.
	status, body = env.httpRequest(http.MethodPost, "/api/auth/totp/disable", session.Token, map[string]string{
		"password": "correct-horse",
	})
	if status != http.StatusNoContent {
		t.Fatalf("disable: status=%d body=%s", status, string(body))
	}

	// Login again should now mint a session token directly.
	status, body = env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "correct-horse",
	})
	if status != http.StatusOK {
		t.Fatalf("login after disable: status=%d body=%s", status, string(body))
	}
	var afterDisable tokenPayload
	env.decodeJSON(body, &afterDisable)
	if afterDisable.Token == "" {
		t.Fatal("login after disable: expected session token, got empty")
	}
}

// TestTOTPRecoveryCode verifies that a recovery code burns once and
// once only on the same /verify endpoint. The TOTP code path is
// exercised in TestTOTPLifecycle; this is the recovery-code branch.
func TestTOTPRecoveryCode(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	// Enroll.
	_, body := env.httpRequest(http.MethodPost, "/api/auth/totp/enroll/begin", tok.Token, nil)
	var begin struct {
		Secret string `json:"secret"`
	}
	env.decodeJSON(body, &begin)
	code, err := totp.GenerateCode(begin.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	_, body = env.httpRequest(http.MethodPost, "/api/auth/totp/enroll/finalize", tok.Token, map[string]string{
		"code": code,
	})
	var finalize struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	env.decodeJSON(body, &finalize)
	if len(finalize.RecoveryCodes) == 0 {
		t.Fatal("no recovery codes returned")
	}

	// Login -> mfa challenge.
	_, body = env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "correct-horse",
	})
	var mfaResp struct {
		MFAToken string `json:"mfa_token"`
	}
	env.decodeJSON(body, &mfaResp)

	// Burn a recovery code: server normalises input to lowercase
	// so any case round-trips.
	rc := strings.ToUpper(finalize.RecoveryCodes[0])
	status, body := env.httpRequest(http.MethodPost, "/api/auth/totp/verify", mfaResp.MFAToken, map[string]string{
		"code": rc,
	})
	if status != http.StatusOK {
		t.Fatalf("recovery code verify: status=%d body=%s", status, string(body))
	}

	// Re-login to get a fresh challenge token, then try the same
	// recovery code: must fail with 401 (single-use semantics).
	_, body = env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "correct-horse",
	})
	env.decodeJSON(body, &mfaResp)
	status, _ = env.httpRequest(http.MethodPost, "/api/auth/totp/verify", mfaResp.MFAToken, map[string]string{
		"code": finalize.RecoveryCodes[0], // same code, lower case this time
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("recovery code re-use: status=%d, want 401", status)
	}
}

// TestMFAChallengeTokenCannotEnroll pins the cross-purpose token
// rejection: an mfa_challenge token must NOT be accepted by the
// session-auth /auth/totp/enroll/* endpoints. The user has to
// satisfy verify first to get a real session token.
func TestMFAChallengeTokenCannotEnroll(t *testing.T) {
	env := setupEnv(t)

	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "correct-horse")

	// Enroll first so the next login returns a challenge token.
	_, body := env.httpRequest(http.MethodPost, "/api/auth/totp/enroll/begin", tok.Token, nil)
	var begin struct {
		Secret string `json:"secret"`
	}
	env.decodeJSON(body, &begin)
	code, _ := totp.GenerateCode(begin.Secret, time.Now())
	env.httpRequest(http.MethodPost, "/api/auth/totp/enroll/finalize", tok.Token, map[string]string{
		"code": code,
	})

	// Login -> challenge token.
	_, body = env.httpRequest(http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "admin@acme.test",
		"password": "correct-horse",
	})
	var mfaResp struct {
		MFAToken string `json:"mfa_token"`
	}
	env.decodeJSON(body, &mfaResp)
	if mfaResp.MFAToken == "" {
		t.Fatal("no challenge token issued")
	}

	// Try to call /enroll/begin (session-auth gate) with a
	// challenge-purpose token: must be 401.
	status, _ := env.httpRequest(http.MethodPost, "/api/auth/totp/enroll/begin", mfaResp.MFAToken, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("challenge token on enroll/begin: status=%d, want 401", status)
	}
}
