package iamcore

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrTokenInvalid is returned by Verifier.Verify for any token that
// fails signature, issuer, audience, or temporal validation. Callers
// (the middleware) map it to HTTP 401 without leaking which specific
// check failed.
var ErrTokenInvalid = errors.New("iamcore: token invalid")

// supportedAlgs is the allow-list of JWS algorithms accepted on
// iam-core access tokens. Restricting the set defends against
// algorithm-confusion attacks (e.g. a token forged with alg=none or
// an HMAC alg verified against a public key). iam-core signs with an
// asymmetric key, so only RSA and ECDSA families are permitted.
var supportedAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512"}

// jwksCacheConfig bounds how long a fetched key set is trusted. The
// effective TTL is taken from the JWKS response's Cache-Control
// max-age, clamped into [minTTL, maxTTL]; when the header is absent
// defaultTTL applies. minRefreshInterval rate-limits on-demand
// refetches triggered by an unknown `kid` so a flood of tokens with
// bogus key ids cannot turn into a fetch storm against iam-core.
type jwksCacheConfig struct {
	minTTL             time.Duration
	maxTTL             time.Duration
	defaultTTL         time.Duration
	minRefreshInterval time.Duration
}

func defaultJWKSCacheConfig() jwksCacheConfig {
	return jwksCacheConfig{
		minTTL:             5 * time.Minute,
		maxTTL:             24 * time.Hour,
		defaultTTL:         1 * time.Hour,
		minRefreshInterval: 1 * time.Minute,
	}
}

// Verifier validates iam-core-issued JWTs. It fetches and caches the
// issuer's JSON Web Key Set, verifies the token signature against the
// key identified by the `kid` header, and enforces the issuer,
// audience, expiry, and not-before claims. A single Verifier is safe
// for concurrent use and is shared across all requests.
type Verifier struct {
	issuer   string
	audience string
	jwksURI  string
	http     *http.Client
	parser   *jwt.Parser

	cfg jwksCacheConfig

	// fetchMu serializes network refreshes so a burst of requests
	// arriving on a cold or stale cache results in a single upstream
	// fetch rather than one per request (thundering-herd guard).
	fetchMu sync.Mutex
	// mu guards the cached key set and the bookkeeping timestamps.
	mu        sync.RWMutex
	keys      map[string]crypto.PublicKey
	expiresAt time.Time
	lastFetch time.Time
}

// NewVerifier constructs a Verifier for the given issuer, audience and
// JWKS endpoint. httpClient may be nil, in which case a client with a
// conservative timeout is used. The key set is fetched lazily on the
// first Verify call (and refreshed on expiry / unknown kid) so server
// start does not block on the JWKS endpoint being reachable.
func NewVerifier(issuer, audience, jwksURI string, httpClient *http.Client) *Verifier {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	issuer = normalizeIssuer(issuer)
	opts := []jwt.ParserOption{
		jwt.WithValidMethods(supportedAlgs),
		jwt.WithExpirationRequired(),
		// Small leeway absorbs clock skew between iam-core and
		// zk-drive without materially widening the validity window.
		jwt.WithLeeway(30 * time.Second),
	}
	if issuer != "" {
		opts = append(opts, jwt.WithIssuer(issuer))
	}
	if audience != "" {
		opts = append(opts, jwt.WithAudience(audience))
	}
	// When audience is empty, aud is intentionally NOT validated (dev/demo
	// convenience). This widens the trust boundary to any token the issuer
	// minted, so cmd/server logs a startup warning in that mode; production
	// deployments must set IAM_CORE_AUDIENCE to pin the relying party.
	return &Verifier{
		issuer:   issuer,
		audience: audience,
		jwksURI:  jwksURI,
		http:     httpClient,
		parser:   jwt.NewParser(opts...),
		cfg:      defaultJWKSCacheConfig(),
	}
}

// Verify parses and validates a raw access token and returns the
// projected Identity. Any validation failure is wrapped in
// ErrTokenInvalid so the caller can respond 401 uniformly.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	claims := &registeredClaims{}
	_, err := v.parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keyForKID(ctx, kid)
	})
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	id := claims.toIdentity()
	if id.Subject == "" {
		return Identity{}, fmt.Errorf("%w: missing subject", ErrTokenInvalid)
	}
	return id, nil
}

// keyForKID returns the public key for the given key id, refreshing
// the JWKS cache when the cache is empty, expired, or does not contain
// the requested kid (key rotation). An empty kid is allowed only when
// the key set holds exactly one key, matching the JWKS spec's
// single-key convenience case.
func (v *Verifier) keyForKID(ctx context.Context, kid string) (crypto.PublicKey, error) {
	if key, ok := v.lookup(kid); ok {
		return key, nil
	}
	if err := v.refresh(ctx, kid); err != nil {
		return nil, err
	}
	if key, ok := v.lookup(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("no key for kid %q", kid)
}

func (v *Verifier) lookup(kid string) (crypto.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.keys) == 0 || time.Now().After(v.expiresAt) {
		return nil, false
	}
	if kid == "" {
		if len(v.keys) == 1 {
			for _, k := range v.keys {
				return k, true
			}
		}
		return nil, false
	}
	k, ok := v.keys[kid]
	return k, ok
}

// refresh fetches the JWKS from iam-core and replaces the cache. It is
// guarded by fetchMu so concurrent callers collapse onto one network
// round-trip; a caller that finds the kid already present (filled by
// the goroutine that won the lock) returns without fetching. The
// wantKID-present fast path also enforces minRefreshInterval so an
// attacker spraying tokens with random kids cannot drive unbounded
// fetches.
func (v *Verifier) refresh(ctx context.Context, wantKID string) error {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()

	// Another goroutine may have refreshed while we waited for the
	// lock — re-check before spending a network call.
	if _, ok := v.lookup(wantKID); ok {
		return nil
	}
	v.mu.RLock()
	lastFetch := v.lastFetch
	cacheValid := len(v.keys) > 0 && time.Now().Before(v.expiresAt)
	v.mu.RUnlock()
	// When the cache is still valid (we're here only because of an
	// unknown kid), throttle refetches to minRefreshInterval.
	if cacheValid && time.Since(lastFetch) < v.cfg.minRefreshInterval {
		return fmt.Errorf("jwks refresh throttled for unknown kid %q", wantKID)
	}

	keys, ttl, err := v.fetchJWKS(ctx)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = keys
	v.expiresAt = time.Now().Add(ttl)
	v.lastFetch = time.Now()
	v.mu.Unlock()
	return nil
}

func (v *Verifier) fetchJWKS(ctx context.Context) (map[string]crypto.PublicKey, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURI, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("iamcore: build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("iamcore: fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, 0, fmt.Errorf("iamcore: jwks endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// Bound the body so a hostile or misconfigured endpoint cannot
	// exhaust memory. A realistic JWKS is a few KB; 1 MiB is generous.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("iamcore: read jwks: %w", err)
	}
	var set jwks
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, 0, fmt.Errorf("iamcore: decode jwks: %w", err)
	}
	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	for i := range set.Keys {
		jwk := set.Keys[i]
		// Only signature keys are eligible to verify a JWT. Skip
		// encryption keys (use="enc") so we never accept a token
		// signed with a key the issuer designated for a different
		// purpose.
		if jwk.Use != "" && jwk.Use != "sig" {
			continue
		}
		key, err := jwk.publicKey()
		if err != nil {
			// Skip individual malformed/unsupported keys rather than
			// failing the whole set — a future key type should not
			// break verification for the keys we do understand.
			continue
		}
		if jwk.Kid == "" {
			// Index unnamed keys under "" so the single-key
			// convenience path in lookup can find them.
			keys[""] = key
			continue
		}
		keys[jwk.Kid] = key
	}
	if len(keys) == 0 {
		return nil, 0, errors.New("iamcore: jwks contained no usable signing keys")
	}
	return keys, v.ttlFromCacheControl(resp.Header.Get("Cache-Control")), nil
}

// ttlFromCacheControl derives the cache lifetime from the response's
// Cache-Control max-age directive, clamped into the configured bounds.
// Absent or unparsable headers fall back to defaultTTL.
func (v *Verifier) ttlFromCacheControl(header string) time.Duration {
	maxAge, ok := parseMaxAge(header)
	if !ok {
		return v.cfg.defaultTTL
	}
	ttl := time.Duration(maxAge) * time.Second
	if ttl < v.cfg.minTTL {
		return v.cfg.minTTL
	}
	if ttl > v.cfg.maxTTL {
		return v.cfg.maxTTL
	}
	return ttl
}

// parseMaxAge extracts the max-age (seconds) from a Cache-Control
// header value. It honors no-store / no-cache by reporting "no usable
// max-age" so the caller falls back to its default TTL rather than
// caching for zero seconds (which would refetch on every request).
func parseMaxAge(header string) (int, bool) {
	if header == "" {
		return 0, false
	}
	for _, part := range strings.Split(header, ",") {
		directive := strings.TrimSpace(strings.ToLower(part))
		switch {
		case directive == "no-store", directive == "no-cache":
			return 0, false
		case strings.HasPrefix(directive, "max-age="):
			v := strings.TrimPrefix(directive, "max-age=")
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// jwks / jsonWebKey model the subset of RFC 7517 that iam-core emits.
// Parsing is done by hand (rather than pulling in a JWKS library) to
// keep the dependency surface minimal, matching the repo convention of
// providing OAuth endpoints inline (see api/auth/oauth.go).
type jwks struct {
	Keys []jsonWebKey `json:"keys"`
}

type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	// RSA parameters.
	N string `json:"n"`
	E string `json:"e"`
	// EC parameters.
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey converts a JWK into a crypto.PublicKey suitable for JWT
// signature verification. RSA and the NIST P-curves are supported,
// matching supportedAlgs.
func (k jsonWebKey) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		return k.rsaPublicKey()
	case "EC":
		return k.ecPublicKey()
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

func (k jsonWebKey) rsaPublicKey() (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode rsa modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode rsa exponent: %w", err)
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errors.New("empty rsa parameters")
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() < 2 {
		return nil, errors.New("invalid rsa exponent")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}

func (k jsonWebKey) ecPublicKey() (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported ec curve %q", k.Crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("decode ec x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("decode ec y: %w", err)
	}
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	if !curve.IsOnCurve(x, y) {
		return nil, errors.New("ec point not on curve")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}
