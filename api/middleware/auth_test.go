package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// stubChecker implements SessionChecker with table-controlled return
// values so the middleware tests can exercise every code path
// (revoked, not-revoked, error, missing iat) without touching Redis.
//
// Captures the last call's arguments so tests can assert the
// middleware passes through claims correctly (workspaceID, userID,
// issuedAt) rather than fabricating its own values.
type stubChecker struct {
	revoked bool
	err     error

	calls           int
	lastWorkspaceID uuid.UUID
	lastUserID      uuid.UUID
	lastIssuedAt    time.Time
}

func (s *stubChecker) IsRevoked(ctx context.Context, workspaceID, userID uuid.UUID, issuedAt time.Time) (bool, error) {
	s.calls++
	s.lastWorkspaceID = workspaceID
	s.lastUserID = userID
	s.lastIssuedAt = issuedAt
	return s.revoked, s.err
}

// TestAuthMiddleware_TableDriven exercises the full auth-gate decision
// tree: missing header, malformed token, expired token, the new
// SessionChecker-revoked path, SessionChecker error path, and the
// happy path. Each case asserts (status, downstream-handler-was-called)
// rather than peeking inside the middleware, so refactors that
// preserve the contract don't churn the test.
func TestAuthMiddleware_TableDriven(t *testing.T) {
	const secret = "test-secret"
	workspaceID := uuid.New()
	userID := uuid.New()

	validToken, _, err := IssueToken(secret, userID, workspaceID, "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue valid token: %v", err)
	}
	// Token signed with a different secret — exercises the
	// "invalid signature" branch in ParseToken.
	wrongSecretToken, _, err := IssueToken("other-secret", userID, workspaceID, "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue wrong-secret token: %v", err)
	}
	// Token issued one hour in the past, expired half an hour ago.
	expiredToken, _, err := IssueToken(secret, userID, workspaceID, "admin", -30*time.Minute)
	if err != nil {
		t.Fatalf("issue expired token: %v", err)
	}

	tests := []struct {
		name       string
		header     string
		checker    SessionChecker
		wantStatus int
		wantNext   bool
	}{
		{
			name:       "missing authorization header",
			header:     "",
			checker:    nil,
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "non-bearer scheme",
			header:     "Basic " + validToken,
			checker:    nil,
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "invalid signature",
			header:     "Bearer " + wrongSecretToken,
			checker:    nil,
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "expired token",
			header:     "Bearer " + expiredToken,
			checker:    nil,
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "valid token, no checker",
			header:     "Bearer " + validToken,
			checker:    nil,
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
		{
			name:       "valid token, checker says not revoked",
			header:     "Bearer " + validToken,
			checker:    &stubChecker{revoked: false},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
		{
			name:       "valid token, checker says revoked",
			header:     "Bearer " + validToken,
			checker:    &stubChecker{revoked: true},
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "valid token, checker errors fails closed",
			header:     "Bearer " + validToken,
			checker:    &stubChecker{err: errors.New("redis down")},
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var nextCalled bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				// Verify identity propagation when next gets called.
				if uid, ok := UserIDFromContext(r.Context()); !ok || uid != userID {
					t.Errorf("UserIDFromContext: got (%v, %v), want (%v, true)", uid, ok, userID)
				}
				w.WriteHeader(http.StatusOK)
			})

			h := AuthMiddleware(secret, tc.checker)(next)
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rr.Code, tc.wantStatus)
			}
			if nextCalled != tc.wantNext {
				t.Errorf("next called: got %v, want %v", nextCalled, tc.wantNext)
			}
			// When the checker was consulted (i.e. token parse
			// succeeded), confirm the middleware forwarded the
			// JWT claims verbatim rather than synthesising them.
			if stub, ok := tc.checker.(*stubChecker); ok && stub.calls > 0 {
				if stub.lastWorkspaceID != workspaceID {
					t.Errorf("checker.workspaceID: got %v, want %v", stub.lastWorkspaceID, workspaceID)
				}
				if stub.lastUserID != userID {
					t.Errorf("checker.userID: got %v, want %v", stub.lastUserID, userID)
				}
				if stub.lastIssuedAt.IsZero() {
					t.Error("checker.issuedAt: got zero time, want valid iat")
				}
			}
		})
	}
}

// slowChecker simulates a Redis call that hangs indefinitely until
// the call context is cancelled. The middleware should detect the
// timeout and fail closed (401) within ~SessionCheckTimeout rather
// than waiting for the full request deadline.
type slowChecker struct{}

func (slowChecker) IsRevoked(ctx context.Context, _, _ uuid.UUID, _ time.Time) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

// TestAuthMiddleware_BoundedTimeoutOnSlowChecker pins the production
// hardening for the "Redis outage causes API hang" failure mode: a
// hanging IsRevoked call must complete within SessionCheckTimeout
// (1s) and respond 401, not hold the request for the full client
// read deadline.
//
// Without the bounded context the goroutine would block on the
// channel receive forever (the slow checker never returns on its
// own), so a timeout on this test itself is the negative signal —
// success means the middleware exited fast.
func TestAuthMiddleware_BoundedTimeoutOnSlowChecker(t *testing.T) {
	t.Parallel()
	const secret = "test-secret"
	token, _, err := IssueToken(secret, uuid.New(), uuid.New(), "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	h := AuthMiddleware(secret, slowChecker{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("next handler should not be called when checker times out")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rr.Code)
	}
	// The middleware's deadline is SessionCheckTimeout. Allow a
	// generous fudge factor for slow CI runners but assert the
	// upper bound stays well below the standard client read
	// deadline (30s).
	if elapsed > SessionCheckTimeout+2*time.Second {
		t.Errorf("middleware took %v on slow checker; expected to fail closed within ~%v", elapsed, SessionCheckTimeout)
	}
}

// TestAuthMiddleware_CheckerOnlyCalledOnce ensures we don't accidentally
// regress to a code path that calls IsRevoked multiple times per
// request (which would inflate Redis load on the hot path).
func TestAuthMiddleware_CheckerOnlyCalledOnce(t *testing.T) {
	const secret = "test-secret"
	token, _, err := IssueToken(secret, uuid.New(), uuid.New(), "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	stub := &stubChecker{revoked: false}
	h := AuthMiddleware(secret, stub)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if stub.calls != 1 {
		t.Errorf("checker call count: got %d, want 1", stub.calls)
	}
}

// TestAuthMiddleware_RejectsPurposeToken pins the TOTP-challenge
// invariant: AuthMiddleware MUST refuse any token whose Purpose
// claim is set, regardless of its TTL or signing key. This is the
// single chokepoint
// that prevents an attacker who captures an mfa_challenge token from
// replaying it against a data-plane endpoint.
func TestAuthMiddleware_RejectsPurposeToken(t *testing.T) {
	t.Parallel()
	const secret = "test-secret"
	for _, purpose := range []string{PurposeMFAChallenge, PurposeMFAEnroll} {
		purpose := purpose
		t.Run(purpose, func(t *testing.T) {
			t.Parallel()
			token, _, err := issueWithPurpose(secret, uuid.New(), uuid.New(), "", purpose, time.Hour)
			if err != nil {
				t.Fatalf("issue purpose token: %v", err)
			}
			h := AuthMiddleware(secret, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Errorf("next handler reached on %s purpose token", purpose)
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s: status got %d, want 401", purpose, rr.Code)
			}
		})
	}
}

// TestPurposeMiddleware exercises the dedicated purpose-token gate
// used by /auth/totp/verify (mfa_challenge) and the must-enroll
// /auth/totp/enroll/*/required routes (mfa_enroll). The contract
// is the inverse of AuthMiddleware: refuse tokens WITHOUT a matching
// purpose, accept tokens that have it.
func TestPurposeMiddleware(t *testing.T) {
	t.Parallel()
	const secret = "test-secret"
	workspaceID := uuid.New()
	userID := uuid.New()

	sessionToken, _, err := IssueToken(secret, userID, workspaceID, "admin", time.Hour)
	if err != nil {
		t.Fatalf("issue session token: %v", err)
	}
	challengeToken, _, err := IssueMFAChallengeToken(secret, userID, workspaceID)
	if err != nil {
		t.Fatalf("issue challenge token: %v", err)
	}
	enrollToken, _, err := IssueMFAEnrollToken(secret, userID, workspaceID)
	if err != nil {
		t.Fatalf("issue enroll token: %v", err)
	}

	tests := []struct {
		name       string
		want       string
		header     string
		wantStatus int
		wantNext   bool
	}{
		{
			name:       "challenge token accepted by challenge gate",
			want:       PurposeMFAChallenge,
			header:     "Bearer " + challengeToken,
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
		{
			name:       "enroll token rejected by challenge gate",
			want:       PurposeMFAChallenge,
			header:     "Bearer " + enrollToken,
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "session token rejected by challenge gate",
			want:       PurposeMFAChallenge,
			header:     "Bearer " + sessionToken,
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "enroll token accepted by enroll gate",
			want:       PurposeMFAEnroll,
			header:     "Bearer " + enrollToken,
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
		{
			name:       "challenge token rejected by enroll gate",
			want:       PurposeMFAEnroll,
			header:     "Bearer " + challengeToken,
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name:       "missing authorization header rejected",
			want:       PurposeMFAChallenge,
			header:     "",
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var nextCalled bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				claims, ok := ClaimsFromContext(r.Context())
				if !ok {
					t.Error("ClaimsFromContext: not found in passing path")
					return
				}
				if claims.Purpose != tc.want {
					t.Errorf("claims.Purpose: got %q, want %q", claims.Purpose, tc.want)
				}
				w.WriteHeader(http.StatusOK)
			})
			h := PurposeMiddleware(secret, tc.want)(next)
			req := httptest.NewRequest(http.MethodPost, "/totp/verify", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rr.Code, tc.wantStatus)
			}
			if nextCalled != tc.wantNext {
				t.Errorf("next called: got %v, want %v", nextCalled, tc.wantNext)
			}
		})
	}
}
