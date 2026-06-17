package iamcore

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	apimw "github.com/kennguy3n/zk-drive/api/middleware"
)

// testJWKS is a minimal in-process JWKS endpoint plus RS256 signer,
// enough to drive Verifier.Reverify through the real parse + key-lookup
// path without stubbing any signature verification.
type testJWKS struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newTestJWKS(t *testing.T) *testJWKS {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	j := &testJWKS{key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := j.key.Public().(*rsa.PublicKey)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": j.kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	j.server = httptest.NewServer(mux)
	t.Cleanup(j.server.Close)
	return j
}

func (j *testJWKS) jwksURI() string { return j.server.URL + "/jwks" }

// mint signs an RS256 token with the given kid header and lifetime
// (negative ttl => already expired).
func (j *testJWKS) mint(t *testing.T, kid string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": "subject-1",
		"iat": now.Add(-time.Minute).Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
		"exp": now.Add(ttl).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(j.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestVerifierReverify(t *testing.T) {
	j := newTestJWKS(t)

	t.Run("valid token returns nil", func(t *testing.T) {
		v := NewVerifier("", "", j.jwksURI(), nil)
		if err := v.Reverify(context.Background(), j.mint(t, j.kid, time.Hour)); err != nil {
			t.Fatalf("Reverify(valid) = %v, want nil", err)
		}
	})

	t.Run("expired token is a definitive rejection", func(t *testing.T) {
		v := NewVerifier("", "", j.jwksURI(), nil)
		err := v.Reverify(context.Background(), j.mint(t, j.kid, -time.Hour))
		if err == nil {
			t.Fatal("Reverify(expired) = nil, want error")
		}
		if errors.Is(err, apimw.ErrReverifyUnavailable) {
			t.Fatalf("expired token classified as transient: %v", err)
		}
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("expired token not wrapped in ErrTokenInvalid: %v", err)
		}
	})

	t.Run("unknown kid is a definitive rejection", func(t *testing.T) {
		v := NewVerifier("", "", j.jwksURI(), nil)
		// JWKS is reachable but does not contain this kid (rotated-out
		// or revoked signing key): definitive, not transient.
		err := v.Reverify(context.Background(), j.mint(t, "rotated-out-kid", time.Hour))
		if err == nil {
			t.Fatal("Reverify(unknown kid) = nil, want error")
		}
		if errors.Is(err, apimw.ErrReverifyUnavailable) {
			t.Fatalf("unknown kid classified as transient: %v", err)
		}
	})

	t.Run("rotated-out kid on a warm cache is a definitive rejection", func(t *testing.T) {
		v := NewVerifier("", "", j.jwksURI(), nil)
		// Warm the cache and stamp lastFetch ~now by reverifying a
		// valid token first. This reproduces the shared-Verifier setup
		// in production: HTTP request traffic keeps the JWKS cache warm,
		// so the collab reauth pump's Reverify of a now-rotated key
		// hits the minRefreshInterval throttle instead of a fresh fetch.
		if err := v.Reverify(context.Background(), j.mint(t, j.kid, time.Hour)); err != nil {
			t.Fatalf("warm-up Reverify(valid) = %v, want nil", err)
		}
		// The kid is absent from the recently-fetched, still-valid key
		// set, so refresh is throttled. That must remain a definitive
		// rejection, not a transient outage — otherwise a federated
		// collab socket signed by a rotated-out key would survive until
		// the throttle window lapses (~minRefreshInterval).
		err := v.Reverify(context.Background(), j.mint(t, "rotated-out-kid", time.Hour))
		if err == nil {
			t.Fatal("Reverify(throttled rotated-out kid) = nil, want error")
		}
		if errors.Is(err, apimw.ErrReverifyUnavailable) {
			t.Fatalf("throttled rotated-out kid classified as transient: %v", err)
		}
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("throttled rotated-out kid not wrapped in ErrTokenInvalid: %v", err)
		}
	})

	t.Run("unreachable JWKS is transient", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURI := dead.URL + "/jwks"
		dead.Close() // refuse connections so the fetch fails at the transport
		v := NewVerifier("", "", deadURI, nil)
		err := v.Reverify(context.Background(), j.mint(t, j.kid, time.Hour))
		if err == nil {
			t.Fatal("Reverify(unreachable JWKS) = nil, want error")
		}
		if !errors.Is(err, apimw.ErrReverifyUnavailable) {
			t.Fatalf("unreachable JWKS not classified as transient: %v", err)
		}
	})
}
