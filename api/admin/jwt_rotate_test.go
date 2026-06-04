package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/admin"
	"github.com/kennguy3n/zk-drive/api/middleware"
	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
)

// fakeJWTRotator implements admin.JWTRotator without a live key store.
// signingAlg is what Algorithm() reports — distinct from the rotated
// key's own algorithm — so the test can assert the rotate response
// surfaces the manager's *effective* signing algorithm.
type fakeJWTRotator struct {
	rec        cryptopkg.SigningKeyRecord
	signingAlg string
	// calls, when non-nil, counts RotateKey invocations so a test can
	// assert the platform-admin gate short-circuits before any key is
	// generated.
	calls *int
}

func (f fakeJWTRotator) RotateKey(context.Context) (cryptopkg.SigningKeyRecord, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.rec, nil
}

func (f fakeJWTRotator) Algorithm() string { return f.signingAlg }

// rotateContextFor synthesizes the authenticated admin context that
// the AdminOnly middleware would normally install (workspace + the
// given actor user id), without running the JWT path.
func rotateContextFor(actor uuid.UUID) context.Context {
	ctx := middleware.WithWorkspaceID(context.Background(), uuid.New())
	ctx = middleware.WithUserID(ctx, actor)
	return ctx
}

// rotateContext is rotateContextFor with a random actor, for tests
// that do not exercise the platform-admin allowlist.
func rotateContext() context.Context {
	return rotateContextFor(uuid.New())
}

func decodeRotate(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode rotate response: %v (%s)", err, body)
	}
	return m
}

// TestRotateJWTKey_ReportsActiveSigningAlgorithm covers the live case:
// the freshly-rotated ES256 key is also the active signing key, so the
// response's signing_algorithm matches the key's algorithm.
func TestRotateJWTKey_ReportsActiveSigningAlgorithm(t *testing.T) {
	keyID := uuid.New()
	adminID := uuid.New()
	h := admin.NewHandler(nil, nil, nil, nil).WithJWTRotator(fakeJWTRotator{
		rec: cryptopkg.SigningKeyRecord{
			ID:        keyID,
			Algorithm: cryptopkg.AlgES256,
			CreatedAt: time.Now().UTC(),
		},
		signingAlg: cryptopkg.AlgES256,
	}).WithPlatformAdmins([]uuid.UUID{adminID})

	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil).WithContext(rotateContextFor(adminID))
	rec := httptest.NewRecorder()
	h.RotateJWTKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeRotate(t, rec.Body.Bytes())
	if got := m["key_id"]; got != keyID.String() {
		t.Errorf("key_id = %v, want %s", got, keyID)
	}
	if got := m["algorithm"]; got != cryptopkg.AlgES256 {
		t.Errorf("algorithm = %v, want ES256", got)
	}
	if got := m["signing_algorithm"]; got != cryptopkg.AlgES256 {
		t.Errorf("signing_algorithm = %v, want ES256", got)
	}
}

// TestRotateJWTKey_ReportsHS256WhenForced covers the pre-provisioning
// case: JWT_ALGORITHM=HS256 forces symmetric signing, so even though an
// ES256 key was just stored (algorithm=ES256), the manager still signs
// with HS256 and the response says so — making clear the key is not
// live yet.
func TestRotateJWTKey_ReportsHS256WhenForced(t *testing.T) {
	adminID := uuid.New()
	h := admin.NewHandler(nil, nil, nil, nil).WithJWTRotator(fakeJWTRotator{
		rec: cryptopkg.SigningKeyRecord{
			ID:        uuid.New(),
			Algorithm: cryptopkg.AlgES256,
			CreatedAt: time.Now().UTC(),
		},
		signingAlg: cryptopkg.AlgHS256,
	}).WithPlatformAdmins([]uuid.UUID{adminID})

	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil).WithContext(rotateContextFor(adminID))
	rec := httptest.NewRecorder()
	h.RotateJWTKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeRotate(t, rec.Body.Bytes())
	if got := m["algorithm"]; got != cryptopkg.AlgES256 {
		t.Errorf("algorithm = %v, want ES256 (key material is ES256)", got)
	}
	if got := m["signing_algorithm"]; got != cryptopkg.AlgHS256 {
		t.Errorf("signing_algorithm = %v, want HS256 (forced, key not live)", got)
	}
}

// TestRotateJWTKey_NotWiredReturns501 confirms the route degrades to 501
// when no rotator is wired (HS256-only deployments), and that the
// typed-nil guard in WithJWTRotator collapses a nil interface back to a
// real nil rather than panicking.
func TestRotateJWTKey_NotWiredReturns501(t *testing.T) {
	h := admin.NewHandler(nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil).WithContext(rotateContext())
	rec := httptest.NewRecorder()
	h.RotateJWTKey(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%s)", rec.Code, rec.Body.String())
	}
}

// TestRotateJWTKey_DeniesNonPlatformAdmin verifies that an authenticated
// workspace admin who is NOT in the platform-admin allowlist is denied
// (403) and that no key is generated — the gate short-circuits before
// RotateKey is ever called, so a workspace admin cannot rotate the
// platform-wide signing key.
func TestRotateJWTKey_DeniesNonPlatformAdmin(t *testing.T) {
	var calls int
	platformAdmin := uuid.New()
	h := admin.NewHandler(nil, nil, nil, nil).WithJWTRotator(fakeJWTRotator{
		rec: cryptopkg.SigningKeyRecord{
			ID:        uuid.New(),
			Algorithm: cryptopkg.AlgES256,
			CreatedAt: time.Now().UTC(),
		},
		signingAlg: cryptopkg.AlgES256,
		calls:      &calls,
	}).WithPlatformAdmins([]uuid.UUID{platformAdmin})

	// Caller is some other workspace admin, not the platform admin.
	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil).WithContext(rotateContextFor(uuid.New()))
	rec := httptest.NewRecorder()
	h.RotateJWTKey(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeRotate(t, rec.Body.Bytes())["code"]; got != string(middleware.ErrCodePlatformAdminOnly) {
		t.Errorf("error code = %v, want %s", got, middleware.ErrCodePlatformAdminOnly)
	}
	if calls != 0 {
		t.Errorf("RotateKey called %d times, want 0 (gate must short-circuit before rotation)", calls)
	}
}

// TestRotateJWTKey_DeniesWhenNoPlatformAdminsConfigured verifies the
// deny-by-default posture: with the rotator wired but PLATFORM_ADMIN_USER_IDS
// unset (empty allowlist), every caller is denied so a brand-new
// deployment cannot rotate the platform key until an operator opts
// specific users in.
func TestRotateJWTKey_DeniesWhenNoPlatformAdminsConfigured(t *testing.T) {
	var calls int
	h := admin.NewHandler(nil, nil, nil, nil).WithJWTRotator(fakeJWTRotator{
		rec: cryptopkg.SigningKeyRecord{
			ID:        uuid.New(),
			Algorithm: cryptopkg.AlgES256,
			CreatedAt: time.Now().UTC(),
		},
		signingAlg: cryptopkg.AlgES256,
		calls:      &calls,
	})

	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil).WithContext(rotateContext())
	rec := httptest.NewRecorder()
	h.RotateJWTKey(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Errorf("RotateKey called %d times, want 0", calls)
	}
}
