package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/session"
)

// stubValidator implements SessionChecker + SessionValidator so the
// session-aware middleware tests can drive each branch (live, anomaly,
// revoked/not-found, store error) without Redis. It records the
// arguments it was called with so tests can assert the middleware
// forwards the right device context (sid, UA, IP).
type stubValidator struct {
	validateErr error

	calls    int
	lastSID  string
	lastUA   string
	lastIP   string
	lastWSID uuid.UUID
}

func (s *stubValidator) IsRevoked(context.Context, uuid.UUID, uuid.UUID, time.Time) (bool, error) {
	return false, nil
}

func (s *stubValidator) ValidateSession(_ context.Context, workspaceID uuid.UUID, sessionID, userAgent, clientIP string) error {
	s.calls++
	s.lastWSID = workspaceID
	s.lastSID = sessionID
	s.lastUA = userAgent
	s.lastIP = clientIP
	return s.validateErr
}

// TestAuthMiddleware_SessionValidation exercises the per-session gate
// added in 6.2: a token carrying a sid is admitted only when the
// validator reports the session live and the device matches; anomaly
// and not-found map to 401 (distinct codes); a store error fails
// closed.
func TestAuthMiddleware_SessionValidation(t *testing.T) {
	const secret = "test-secret"
	userID, wsID := uuid.New(), uuid.New()

	tokenWithSID := func(sid string) string {
		tok, _, err := IssueSessionTokenWith(hmacSigner{secret}, userID, wsID, "admin", sid, time.Hour)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		return tok
	}

	cases := []struct {
		name       string
		sid        string
		validate   error
		wantStatus int
		wantNext   bool
	}{
		{"live session admitted", "sess-1", nil, http.StatusOK, true},
		{"device anomaly blocked", "sess-1", session.ErrSessionAnomaly, http.StatusUnauthorized, false},
		{"revoked session blocked", "sess-1", session.ErrSessionNotFound, http.StatusUnauthorized, false},
		{"store error fails closed", "sess-1", context.DeadlineExceeded, http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sv := &stubValidator{validateErr: tc.validate}
			nextCalled := false
			h := AuthMiddlewareWithSessions(hmacSigner{secret}, sv, sv, 0)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+tokenWithSID(tc.sid))
			req.Header.Set("User-Agent", "Mozilla/5.0 Test")
			req.RemoteAddr = "203.0.113.7:44321"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d", rec.Code, tc.wantStatus)
			}
			if nextCalled != tc.wantNext {
				t.Errorf("next called: got %v want %v", nextCalled, tc.wantNext)
			}
			if sv.calls != 1 {
				t.Errorf("validator calls: got %d want 1", sv.calls)
			}
			if sv.lastSID != tc.sid || sv.lastWSID != wsID {
				t.Errorf("forwarded sid/ws mismatch: sid=%q ws=%s", sv.lastSID, sv.lastWSID)
			}
			if sv.lastUA != "Mozilla/5.0 Test" || sv.lastIP != "203.0.113.7" {
				t.Errorf("forwarded device mismatch: ua=%q ip=%q", sv.lastUA, sv.lastIP)
			}
		})
	}
}

// TestAuthMiddleware_NoSIDSkipsValidator pins that a token without a
// sid (legacy / pre-6.2) never invokes the validator — the per-session
// checks are strictly additive and must not break older tokens.
func TestAuthMiddleware_NoSIDSkipsValidator(t *testing.T) {
	const secret = "test-secret"
	sv := &stubValidator{validateErr: session.ErrSessionAnomaly}

	tok, _, err := IssueTokenWith(hmacSigner{secret}, uuid.New(), uuid.New(), "member", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	nextCalled := false
	h := AuthMiddlewareWithSessions(hmacSigner{secret}, sv, sv, 0)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !nextCalled || rec.Code != http.StatusOK {
		t.Fatalf("legacy token must pass: next=%v status=%d", nextCalled, rec.Code)
	}
	if sv.calls != 0 {
		t.Fatalf("validator must not run for a sid-less token, ran %d times", sv.calls)
	}
}

// TestAuthMiddleware_NilValidatorSkipsSessionCheck pins that wiring a
// nil validator (stateless / in-memory dev path) disables per-session
// enforcement even for a token that carries a sid.
func TestAuthMiddleware_NilValidatorSkipsSessionCheck(t *testing.T) {
	const secret = "test-secret"
	tok, _, err := IssueSessionTokenWith(hmacSigner{secret}, uuid.New(), uuid.New(), "member", "sess-9", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	nextCalled := false
	h := AuthMiddlewareWithSessions(hmacSigner{secret}, nil, nil, 0)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !nextCalled || rec.Code != http.StatusOK {
		t.Fatalf("nil validator must skip session check: next=%v status=%d", nextCalled, rec.Code)
	}
}
