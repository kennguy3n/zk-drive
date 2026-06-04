package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/middleware"
	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
	platformsvc "github.com/kennguy3n/zk-drive/internal/platform"
)

// TestDecodeOptional covers the optional-body decode used by
// SuspendWorkspace. A real body must always be decoded (even when the
// transport reports ContentLength == -1, as HTTP/2 and chunked requests
// do), an empty body must be tolerated as "no body", and a non-empty
// malformed body must still produce a 400.
func TestDecodeOptional(t *testing.T) {
	type body struct {
		Reason string `json:"reason"`
	}

	t.Run("valid body with unknown ContentLength is decoded", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"reason":"abuse"}`))
		// Simulate HTTP/2 / chunked: length unknown despite a real body.
		r.ContentLength = -1
		w := httptest.NewRecorder()

		var dst body
		if !decodeOptional(w, r, &dst) {
			t.Fatalf("expected decodeOptional to succeed, got status %d", w.Code)
		}
		if dst.Reason != "abuse" {
			t.Fatalf("body not decoded: %+v", dst)
		}
	})

	t.Run("empty body is tolerated", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
		r.ContentLength = -1
		w := httptest.NewRecorder()

		var dst body
		if !decodeOptional(w, r, &dst) {
			t.Fatalf("expected empty body to be tolerated, got status %d", w.Code)
		}
		if dst.Reason != "" {
			t.Fatalf("expected zero-value body, got %+v", dst)
		}
	})

	t.Run("malformed body is rejected with 400", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"reason":`))
		w := httptest.NewRecorder()

		var dst body
		if decodeOptional(w, r, &dst) {
			t.Fatalf("expected malformed body to be rejected")
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed body, got %d", w.Code)
		}
	})
}

// fakeRotator is an in-memory JWTRotator that records calls and returns
// a canned platform-wide signing-key record.
type fakeRotator struct {
	calls      int
	rec        cryptopkg.SigningKeyRecord
	signingAlg string
}

func (f *fakeRotator) RotateKey(ctx context.Context) (cryptopkg.SigningKeyRecord, error) {
	f.calls++
	return f.rec, nil
}

func (f *fakeRotator) Algorithm() string {
	if f.signingAlg == "" {
		return "ES256"
	}
	return f.signingAlg
}

// fakePrincipal carries a fixed permission set, satisfying
// middleware.PlatformPrincipal.
type fakePrincipal struct{ perms map[string]bool }

func (p fakePrincipal) HasPermission(perm string) bool { return p.perms[perm] }

// fakeAuth authenticates every request as the configured principal,
// so tests exercise the real PlatformAuth + RequirePlatformPermission
// chain without a database-backed APIKeyStore.
type fakeAuth struct{ principal middleware.PlatformPrincipal }

func (a fakeAuth) AuthenticateKey(ctx context.Context, presented string) (middleware.PlatformPrincipal, error) {
	return a.principal, nil
}

// mountRotate builds a router that mounts the platform routes behind a
// PlatformAuth that authenticates as a principal with the given
// permissions. rotator may be nil to exercise the "not wired" path.
func mountRotate(rotator JWTRotator, perms ...string) http.Handler {
	set := map[string]bool{}
	for _, p := range perms {
		set[p] = true
	}
	h := NewHandler(nil, nil)
	if rotator != nil {
		h = h.WithJWTRotator(rotator)
	}
	r := chi.NewRouter()
	r.Use(middleware.PlatformAuth(fakeAuth{principal: fakePrincipal{perms: set}}))
	h.RegisterRoutes(r)
	return r
}

func postRotate(t *testing.T, srv http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil)
	req.Header.Set("Authorization", "Bearer pk_test")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestRotateJWTKey_RequiresKeysManage is the core regression: fleet-wide
// JWT rotation must be gated on the keys:manage capability and must not
// rotate when the caller lacks it or the rotator is unwired.
func TestRotateJWTKey_RequiresKeysManage(t *testing.T) {
	rec := cryptopkg.SigningKeyRecord{
		ID:        uuid.New(),
		Algorithm: "ES256",
		CreatedAt: time.Now().UTC(),
	}

	t.Run("with keys:manage rotates", func(t *testing.T) {
		// signingAlg HS256 models the rotated ES256 key being stored but
		// not yet signing (algorithm pinned to HS256) so the response must
		// surface both the key's own algo and the effective signing algo.
		rot := &fakeRotator{rec: rec, signingAlg: "HS256"}
		resp := postRotate(t, mountRotate(rot, platformsvc.PermKeysManage))
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
		}
		if rot.calls != 1 {
			t.Fatalf("RotateKey calls = %d, want 1", rot.calls)
		}
		var body jwtRotateResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.KeyID != rec.ID.String() || body.Algorithm != "ES256" {
			t.Fatalf("unexpected body: %+v", body)
		}
		if body.SigningAlgorithm != "HS256" {
			t.Fatalf("SigningAlgorithm = %q, want HS256 (effective signing algo, distinct from rotated key algo)", body.SigningAlgorithm)
		}
	})

	t.Run("without keys:manage is forbidden and does not rotate", func(t *testing.T) {
		rot := &fakeRotator{rec: rec}
		// A principal with an unrelated capability (tenant:write) must
		// not be able to rotate the fleet-wide signing key.
		resp := postRotate(t, mountRotate(rot, platformsvc.PermTenantWrite))
		if resp.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", resp.Code, resp.Body.String())
		}
		if rot.calls != 0 {
			t.Fatalf("RotateKey calls = %d, want 0 (must reject before rotating)", rot.calls)
		}
	})

	t.Run("not wired responds 501", func(t *testing.T) {
		resp := postRotate(t, mountRotate(nil, platformsvc.PermKeysManage))
		if resp.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501; body=%s", resp.Code, resp.Body.String())
		}
	})

	// A typed-nil *crypto.KeyManager wrapped in the JWTRotator interface
	// is != nil, so mountRotate calls WithJWTRotator with it. The setter
	// must collapse it back to a real nil; otherwise RotateJWTKey would
	// pass its nil-guard and NPE on RotateKey. Expect a clean 501.
	t.Run("typed-nil rotator collapses to 501", func(t *testing.T) {
		var km *cryptopkg.KeyManager
		resp := postRotate(t, mountRotate(km, platformsvc.PermKeysManage))
		if resp.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501; body=%s", resp.Code, resp.Body.String())
		}
	})
}
