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

// fakeRotator is an in-memory JWTRotator that records calls and returns
// a canned platform-wide signing-key record.
type fakeRotator struct {
	calls int
	rec   cryptopkg.SigningKeyRecord
}

func (f *fakeRotator) RotateKey(ctx context.Context) (cryptopkg.SigningKeyRecord, error) {
	f.calls++
	return f.rec, nil
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

// TestRotateJWTKey_RequiresKeysManage is the core regression test for
// the privilege-escalation fix: JWT signing-key rotation is fleet-wide
// and must be gated on the platform keys:manage capability, never the
// per-workspace admin API. A principal lacking keys:manage must be
// rejected before the key is ever rotated.
func TestRotateJWTKey_RequiresKeysManage(t *testing.T) {
	rec := cryptopkg.SigningKeyRecord{
		ID:        uuid.New(),
		Algorithm: "ES256",
		CreatedAt: time.Now().UTC(),
	}

	t.Run("with keys:manage rotates", func(t *testing.T) {
		rot := &fakeRotator{rec: rec}
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

// TestDecodeOptional covers the optional-body decode used by endpoints
// like SuspendWorkspace. The bug it guards against: a bodyless POST with
// no Content-Length header arrives with http.Request.ContentLength == -1,
// which the old `if r.ContentLength != 0` guard treated as "has a body",
// then failed to JSON-decode the empty body and returned a spurious 400.
func TestDecodeOptional(t *testing.T) {
	type body struct {
		Reason string `json:"reason"`
	}

	t.Run("empty body with absent Content-Length (-1) succeeds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/x", http.NoBody)
		req.ContentLength = -1 // simulate a missing Content-Length header
		rec := httptest.NewRecorder()
		var dst body
		if !decodeOptional(rec, req, &dst) {
			t.Fatalf("decodeOptional = false for empty body; code=%d body=%s", rec.Code, rec.Body.String())
		}
		if dst.Reason != "" {
			t.Errorf("reason = %q, want empty", dst.Reason)
		}
	})

	t.Run("empty body with Content-Length 0 succeeds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
		rec := httptest.NewRecorder()
		var dst body
		if !decodeOptional(rec, req, &dst) {
			t.Fatalf("decodeOptional = false for empty body; code=%d", rec.Code)
		}
	})

	t.Run("valid JSON body decodes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"reason":"abuse"}`))
		rec := httptest.NewRecorder()
		var dst body
		if !decodeOptional(rec, req, &dst) {
			t.Fatalf("decodeOptional = false for valid body; code=%d", rec.Code)
		}
		if dst.Reason != "abuse" {
			t.Errorf("reason = %q, want abuse", dst.Reason)
		}
	})

	t.Run("malformed non-empty body is rejected with 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{not json`))
		rec := httptest.NewRecorder()
		var dst body
		if decodeOptional(rec, req, &dst) {
			t.Fatal("decodeOptional = true for malformed body, want false")
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}
