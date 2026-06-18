package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/kennguy3n/zk-drive/api/middleware"
	"github.com/kennguy3n/zk-drive/internal/collab"
	"github.com/kennguy3n/zk-drive/internal/session"
)

// fakeSessionChecker / fakeSessionValidator satisfy the middleware
// SessionChecker / SessionValidator interfaces with canned answers so
// evaluateCollabAuth can be exercised without a real store.
type fakeSessionChecker struct {
	revoked bool
	err     error
}

func (f fakeSessionChecker) IsRevoked(_ context.Context, _, _ uuid.UUID, _ time.Time) (bool, error) {
	return f.revoked, f.err
}

type fakeSessionValidator struct {
	err error
}

func (f fakeSessionValidator) ValidateSession(_ context.Context, _ uuid.UUID, _, _, _ string) error {
	return f.err
}

// fakeReverifier satisfies middleware.TokenReverifier with a canned
// answer so the federated-socket reverify step can be exercised
// without a real iam-core verifier or JWKS endpoint.
type fakeReverifier struct {
	err error
}

func (f fakeReverifier) Reverify(_ context.Context, _ string) error {
	return f.err
}

// fakeCollabRefresher satisfies middleware.CollabTokenRefresher with a
// canned answer so evaluateCollabRefresh can be exercised without a
// real Signer.
type fakeCollabRefresher struct {
	out middleware.CollabAuthRefresh
	err error
}

func (f fakeCollabRefresher) RefreshCollabAuth(_ context.Context, _ string) (middleware.CollabAuthRefresh, error) {
	return f.out, f.err
}

func TestEvaluateCollabRefresh(t *testing.T) {
	ws := uuid.New()
	user := uuid.New()
	iat := time.Now().Add(-time.Minute)
	exp := time.Now().Add(time.Hour)

	t.Run("empty token rejected without consulting refresher", func(t *testing.T) {
		_, err := evaluateCollabRefresh(context.Background(), fakeCollabRefresher{err: errors.New("must not be called")}, ws, user, "")
		if err == nil {
			t.Fatal("expected error for empty in-band token")
		}
	})

	t.Run("refresher error propagates", func(t *testing.T) {
		_, err := evaluateCollabRefresh(context.Background(), fakeCollabRefresher{err: errors.New("bad signature")}, ws, user, "tok")
		if err == nil {
			t.Fatal("expected refresher error to propagate")
		}
	})

	t.Run("workspace mismatch rejected", func(t *testing.T) {
		out := middleware.CollabAuthRefresh{WorkspaceID: uuid.New(), UserID: user, HasExpiry: true, ExpiresAt: exp}
		_, err := evaluateCollabRefresh(context.Background(), fakeCollabRefresher{out: out}, ws, user, "tok")
		if err == nil {
			t.Fatal("expected workspace-mismatch rejection")
		}
	})

	t.Run("user mismatch rejected", func(t *testing.T) {
		out := middleware.CollabAuthRefresh{WorkspaceID: ws, UserID: uuid.New(), HasExpiry: true, ExpiresAt: exp}
		_, err := evaluateCollabRefresh(context.Background(), fakeCollabRefresher{out: out}, ws, user, "tok")
		if err == nil {
			t.Fatal("expected user-mismatch rejection")
		}
	})

	t.Run("same principal accepted and fully mapped", func(t *testing.T) {
		out := middleware.CollabAuthRefresh{
			WorkspaceID: ws,
			UserID:      user,
			SessionID:   "sid-2",
			IssuedAt:    iat,
			ExpiresAt:   exp,
			HasExpiry:   true,
		}
		got, err := evaluateCollabRefresh(context.Background(), fakeCollabRefresher{out: out}, ws, user, "tok")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.hasExpiry || !got.expiresAt.Equal(exp) || !got.issuedAt.Equal(iat) {
			t.Fatalf("expiry/issued not mapped: %+v", got)
		}
		if got.sessionID != "sid-2" {
			t.Fatalf("sessionID = %q, want sid-2", got.sessionID)
		}
		if got.rawToken != "tok" {
			t.Fatalf("rawToken = %q, want tok (the raw frame token)", got.rawToken)
		}
	})
}

func TestEvaluateCollabAuth(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	withSID := func(st collabAuthState) collabAuthState { st.sessionID = "sid-1"; return st }
	withToken := func(st collabAuthState) collabAuthState { st.rawToken = "raw-token"; return st }

	cases := []struct {
		name       string
		state      collabAuthState
		checker    *fakeSessionChecker
		validator  *fakeSessionValidator
		reverifier *fakeReverifier
		wantClose  bool
		wantCode   int
		wantReason string
		wantTrans  bool
	}{
		{
			name:  "valid token, no store",
			state: collabAuthState{hasExpiry: true, expiresAt: future},
		},
		{
			name:       "expired token",
			state:      collabAuthState{hasExpiry: true, expiresAt: past},
			wantClose:  true,
			wantCode:   collabCloseReauthRequired,
			wantReason: "token expired",
		},
		{
			name:       "expiry beats revocation",
			state:      collabAuthState{hasExpiry: true, expiresAt: past},
			checker:    &fakeSessionChecker{revoked: true},
			wantClose:  true,
			wantCode:   collabCloseReauthRequired,
			wantReason: "token expired",
		},
		{
			name:       "revoked by checker",
			state:      collabAuthState{hasExpiry: true, expiresAt: future},
			checker:    &fakeSessionChecker{revoked: true},
			wantClose:  true,
			wantCode:   collabCloseSessionRevoked,
			wantReason: "session revoked",
		},
		{
			name:      "checker transient error keeps session open",
			state:     collabAuthState{hasExpiry: true, expiresAt: future},
			checker:   &fakeSessionChecker{err: errors.New("redis down")},
			wantTrans: true,
		},
		{
			name:       "session not found",
			state:      withSID(collabAuthState{hasExpiry: true, expiresAt: future}),
			validator:  &fakeSessionValidator{err: session.ErrSessionNotFound},
			wantClose:  true,
			wantCode:   collabCloseSessionRevoked,
			wantReason: "session revoked",
		},
		{
			name:       "device anomaly",
			state:      withSID(collabAuthState{hasExpiry: true, expiresAt: future}),
			validator:  &fakeSessionValidator{err: session.ErrSessionAnomaly},
			wantClose:  true,
			wantCode:   collabCloseSessionRevoked,
			wantReason: "device changed",
		},
		{
			name:      "validator transient error keeps session open",
			state:     withSID(collabAuthState{hasExpiry: true, expiresAt: future}),
			validator: &fakeSessionValidator{err: errors.New("redis down")},
			wantTrans: true,
		},
		{
			name:      "validator skipped without sid",
			state:     collabAuthState{hasExpiry: true, expiresAt: future},
			validator: &fakeSessionValidator{err: session.ErrSessionNotFound},
		},
		{
			name:  "no expiry, no store, stays open",
			state: collabAuthState{},
		},
		{
			name:       "reverify valid stays open",
			state:      withToken(collabAuthState{hasExpiry: true, expiresAt: future}),
			reverifier: &fakeReverifier{},
		},
		{
			name:       "reverify failure closes with retriable code",
			state:      withToken(collabAuthState{hasExpiry: true, expiresAt: future}),
			reverifier: &fakeReverifier{err: errors.New("signing key revoked")},
			wantClose:  true,
			wantCode:   collabCloseReauthRequired,
			wantReason: "token reverification failed",
		},
		{
			name:       "reverify unavailable keeps session open",
			state:      withToken(collabAuthState{hasExpiry: true, expiresAt: future}),
			reverifier: &fakeReverifier{err: fmt.Errorf("jwks unreachable: %w", middleware.ErrReverifyUnavailable)},
			wantTrans:  true,
		},
		{
			name:       "reverify skipped without raw token",
			state:      collabAuthState{hasExpiry: true, expiresAt: future},
			reverifier: &fakeReverifier{err: errors.New("would close if it ran")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var checker middleware.SessionChecker
			if tc.checker != nil {
				checker = *tc.checker
			}
			var validator middleware.SessionValidator
			if tc.validator != nil {
				validator = *tc.validator
			}
			var reverifier middleware.TokenReverifier
			if tc.reverifier != nil {
				reverifier = *tc.reverifier
			}
			d := evaluateCollabAuth(context.Background(), tc.state, checker, validator, reverifier)
			if (d.transient != nil) != tc.wantTrans {
				t.Fatalf("transient = %v, want %v", d.transient, tc.wantTrans)
			}
			if d.closeConn != tc.wantClose {
				t.Fatalf("closeConn = %v, want %v", d.closeConn, tc.wantClose)
			}
			if tc.wantClose {
				if d.code != tc.wantCode {
					t.Fatalf("code = %d, want %d", d.code, tc.wantCode)
				}
				if d.reason != tc.wantReason {
					t.Fatalf("reason = %q, want %q", d.reason, tc.wantReason)
				}
			}
		})
	}
}

// dialPumpClose stands up a tiny WS server whose only job is to run
// collabAuthPump over an upgraded connection with the given state, then
// dials it and returns the close code the server sent (or fails).
func dialPumpClose(t *testing.T, st collabAuthState, checker middleware.SessionChecker, validator middleware.SessionValidator, reverifier middleware.TokenReverifier) int {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := collab.NewClient(uuid.New(), uuid.New(), uuid.New(), true, collab.Capability{})
		collabAuthPump(client, conn, st, checker, validator, reverifier, nil, 2*time.Millisecond, logger)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = c.ReadMessage()
	ce := &websocket.CloseError{}
	if !errors.As(err, &ce) {
		t.Fatalf("expected websocket close error, got %v", err)
	}
	return ce.Code
}

// dialPumpRefreshClose stands up a WS server that pre-loads a single
// in-band refresh onto the auth pump's channel before the pump runs,
// then returns the close code the server sends. The pump consumes the
// refresh (the only select case ready at t=0) before its first tick,
// so the close is driven entirely by the applied refresh — no
// client-side timing.
func dialPumpRefreshClose(t *testing.T, st collabAuthState, refresh collabAuthRefresh) int {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := collab.NewClient(uuid.New(), uuid.New(), uuid.New(), true, collab.Capability{})
		refreshCh := make(chan collabAuthRefresh, 1)
		refreshCh <- refresh
		collabAuthPump(client, conn, st, nil, nil, nil, refreshCh, 2*time.Millisecond, logger)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = c.ReadMessage()
	ce := &websocket.CloseError{}
	if !errors.As(err, &ce) {
		t.Fatalf("expected websocket close error, got %v", err)
	}
	return ce.Code
}

func TestCollabAuthPump_AppliesInbandRefreshExpiry(t *testing.T) {
	// The socket starts with no enforced expiry, so it would never close
	// on its own. An in-band refresh carrying an already-past expiry must
	// be applied and tear the socket down with the retriable reauth code
	// on the next tick — proving the read pump → auth pump refresh
	// hand-off advances the enforced auth state.
	refresh := collabAuthRefresh{hasExpiry: true, expiresAt: time.Now().Add(-time.Minute)}
	code := dialPumpRefreshClose(t, collabAuthState{}, refresh)
	if code != collabCloseReauthRequired {
		t.Fatalf("close code = %d, want %d (retriable)", code, collabCloseReauthRequired)
	}
}

func TestCollabAuthPump_ClosesExpiredWithRetriableCode(t *testing.T) {
	code := dialPumpClose(t, collabAuthState{hasExpiry: true, expiresAt: time.Now().Add(-time.Minute)}, nil, nil, nil)
	if code != collabCloseReauthRequired {
		t.Fatalf("close code = %d, want %d (retriable)", code, collabCloseReauthRequired)
	}
	// The retriable code must NOT be one the frontend treats as
	// permanent, so the client reconnects with a fresh token.
	for _, permanent := range []int{1000, 1008, 4001, 4003} {
		if code == permanent {
			t.Fatalf("expiry close code %d is a permanent code; client would not reconnect", code)
		}
	}
}

func TestCollabAuthPump_ClosesRevokedWithPermanentCode(t *testing.T) {
	st := collabAuthState{hasExpiry: true, expiresAt: time.Now().Add(time.Hour)}
	code := dialPumpClose(t, st, fakeSessionChecker{revoked: true}, nil, nil)
	if code != collabCloseSessionRevoked {
		t.Fatalf("close code = %d, want %d (permanent)", code, collabCloseSessionRevoked)
	}
}

func TestCollabAuthPump_ClosesReverifyFailureWithRetriableCode(t *testing.T) {
	// A federated socket whose token the issuer no longer validates is
	// torn down with the retriable reauth code so the client reconnects
	// with a fresh token, exactly as for local expiry.
	st := collabAuthState{rawToken: "raw-token"}
	code := dialPumpClose(t, st, nil, nil, fakeReverifier{err: errors.New("signing key revoked")})
	if code != collabCloseReauthRequired {
		t.Fatalf("close code = %d, want %d (retriable)", code, collabCloseReauthRequired)
	}
	for _, permanent := range []int{1000, 1008, 4001, 4003} {
		if code == permanent {
			t.Fatalf("reverify-failure close code %d is permanent; client would not reconnect", code)
		}
	}
}
