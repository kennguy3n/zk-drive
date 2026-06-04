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
}

func (f fakeJWTRotator) RotateKey(context.Context) (cryptopkg.SigningKeyRecord, error) {
	return f.rec, nil
}

func (f fakeJWTRotator) Algorithm() string { return f.signingAlg }

// rotateContext synthesizes the authenticated admin context that the
// AdminOnly middleware would normally install, without running the JWT
// path.
func rotateContext() context.Context {
	ctx := middleware.WithWorkspaceID(context.Background(), uuid.New())
	ctx = middleware.WithUserID(ctx, uuid.New())
	return ctx
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
	h := admin.NewHandler(nil, nil, nil, nil).WithJWTRotator(fakeJWTRotator{
		rec: cryptopkg.SigningKeyRecord{
			ID:        keyID,
			Algorithm: cryptopkg.AlgES256,
			CreatedAt: time.Now().UTC(),
		},
		signingAlg: cryptopkg.AlgES256,
	})

	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil).WithContext(rotateContext())
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
	h := admin.NewHandler(nil, nil, nil, nil).WithJWTRotator(fakeJWTRotator{
		rec: cryptopkg.SigningKeyRecord{
			ID:        uuid.New(),
			Algorithm: cryptopkg.AlgES256,
			CreatedAt: time.Now().UTC(),
		},
		signingAlg: cryptopkg.AlgHS256,
	})

	req := httptest.NewRequest(http.MethodPost, "/jwt/rotate", nil).WithContext(rotateContext())
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
