package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionTokenRefresher_RefreshCollabAuth(t *testing.T) {
	const secret = "test-secret"
	signer := hmacSigner{secret}
	refresher := NewSessionTokenRefresher(signer)
	userID := uuid.New()
	wsID := uuid.New()

	t.Run("valid session token projects principal and expiry", func(t *testing.T) {
		tok, exp, err := IssueSessionTokenWith(signer, userID, wsID, "admin", "sid-7", time.Hour)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		got, err := refresher.RefreshCollabAuth(context.Background(), tok)
		if err != nil {
			t.Fatalf("RefreshCollabAuth: %v", err)
		}
		if got.WorkspaceID != wsID || got.UserID != userID {
			t.Fatalf("principal = (ws=%s user=%s), want (ws=%s user=%s)", got.WorkspaceID, got.UserID, wsID, userID)
		}
		if got.SessionID != "sid-7" {
			t.Fatalf("SessionID = %q, want sid-7", got.SessionID)
		}
		if !got.HasExpiry {
			t.Fatal("HasExpiry = false, want true for a session token")
		}
		// jwt stores exp at second precision, so compare within a second.
		if d := got.ExpiresAt.Sub(exp); d > time.Second || d < -time.Second {
			t.Fatalf("ExpiresAt = %v, want ~%v", got.ExpiresAt, exp)
		}
		if got.IssuedAt.IsZero() {
			t.Fatal("IssuedAt is zero, want the token's iat")
		}
	})

	t.Run("purpose-scoped token rejected", func(t *testing.T) {
		// An mfa-enroll token must never (re-)authorize a data-plane
		// socket, exactly as AuthMiddleware refuses one on the HTTP path.
		tok, _, err := IssueMFAEnrollTokenWith(signer, userID, wsID)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, err := refresher.RefreshCollabAuth(context.Background(), tok); err == nil {
			t.Fatal("expected purpose-scoped token to be rejected")
		}
	})

	t.Run("malformed token rejected", func(t *testing.T) {
		if _, err := refresher.RefreshCollabAuth(context.Background(), "not-a-jwt"); err == nil {
			t.Fatal("expected malformed token to be rejected")
		}
	})

	t.Run("token signed by a different secret rejected", func(t *testing.T) {
		other, _, err := IssueTokenWith(hmacSigner{"other-secret"}, userID, wsID, "member", time.Hour)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, err := refresher.RefreshCollabAuth(context.Background(), other); err == nil {
			t.Fatal("expected foreign-signed token to be rejected")
		}
	})
}
