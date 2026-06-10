// Package iamcore integrates zk-drive with iam-core (uneycom/iam-core)
// as an OAuth2 / OIDC identity provider. When configured, zk-drive
// stops issuing its own session JWTs and instead becomes a standard
// OAuth2 client of iam-core: the browser runs an Authorization Code +
// PKCE flow against iam-core's Universal Login, and every /api/*
// request carries an iam-core-issued access token that this package
// verifies against iam-core's JWKS.
//
// The integration is OPTIONAL. When IAM_CORE_ISSUER_URL is empty the
// server falls back to the built-in auth stack (api/auth, internal/
// session, internal/totp) so dev/demo deployments keep working with
// no external identity provider. See docs/IAM_CORE.md.
package iamcore

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultScopes are requested when IAM_CORE_SCOPES is unset. openid is
// mandatory for OIDC; email/profile populate the user model; and
// offline_access asks iam-core for a refresh token so the SPA can
// perform silent token renewal without bouncing the user through
// Universal Login again.
var DefaultScopes = []string{"openid", "email", "profile", "offline_access"}

// Config holds the iam-core OAuth2/OIDC client settings. It is built
// from the IAM_CORE_* environment variables in internal/config and
// passed to NewClient / NewVerifier at server start.
type Config struct {
	// IssuerURL is the iam-core OIDC issuer (e.g.
	// https://id.example.com). OIDC discovery is performed at
	// IssuerURL + "/.well-known/openid-configuration" and the issuer
	// value returned there MUST match this URL. An empty IssuerURL
	// disables iam-core auth entirely (built-in fallback).
	IssuerURL string

	// ClientID is the OAuth2 client identifier registered with
	// iam-core for this zk-drive deployment. Sent on the authorize
	// redirect and the token exchange.
	ClientID string

	// ClientSecret authenticates confidential token-endpoint calls
	// (server-side code exchange / refresh). It is OPTIONAL: a pure
	// SPA (public client) completes the PKCE flow in the browser with
	// no secret, in which case this is left empty and never exposed
	// to the frontend.
	ClientSecret string

	// Audience is the expected `aud` claim on access tokens. Verified
	// on every request so a token minted for a different relying
	// party cannot be replayed against zk-drive. When empty, audience
	// validation is skipped (NOT recommended for production).
	Audience string

	// Scopes requested on the authorize redirect. Defaults to
	// DefaultScopes when empty.
	Scopes []string

	// CallbackURL is the OAuth2 redirect_uri — the zk-drive frontend
	// route that receives the authorization code (e.g.
	// https://drive.example.com/auth/callback). Registered with
	// iam-core and echoed on the token exchange.
	CallbackURL string
}

// Enabled reports whether iam-core auth is active. The server wires
// the iam-core middleware when this is true and the built-in auth
// stack otherwise.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.IssuerURL) != ""
}

// EffectiveScopes returns the configured scopes, or DefaultScopes when
// none were set. openid is always present in the result so the flow
// stays a valid OIDC request even if an operator overrides the list.
func (c Config) EffectiveScopes() []string {
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	hasOpenID := false
	for _, s := range scopes {
		if s == "openid" {
			hasOpenID = true
			break
		}
	}
	if !hasOpenID {
		// Prepend rather than append so "openid" leads the
		// space-delimited scope parameter, matching the convention
		// every OIDC provider documents.
		scopes = append([]string{"openid"}, scopes...)
	}
	return scopes
}

// Validate checks that an enabled Config carries the mandatory fields
// and that the URL-typed fields parse as absolute https/http URLs.
// Called at server start so a misconfigured deployment fails fast and
// closed rather than booting into a half-configured auth state.
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if err := validateAbsoluteURL("IAM_CORE_ISSUER_URL", c.IssuerURL); err != nil {
		return err
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return fmt.Errorf("iamcore: IAM_CORE_CLIENT_ID is required when IAM_CORE_ISSUER_URL is set")
	}
	if strings.TrimSpace(c.CallbackURL) == "" {
		return fmt.Errorf("iamcore: IAM_CORE_CALLBACK_URL is required when IAM_CORE_ISSUER_URL is set")
	}
	if err := validateAbsoluteURL("IAM_CORE_CALLBACK_URL", c.CallbackURL); err != nil {
		return err
	}
	return nil
}

func validateAbsoluteURL(name, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("iamcore: %s is not a valid URL: %w", name, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("iamcore: %s must be an absolute URL (got %q)", name, raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("iamcore: %s must use http or https (got scheme %q)", name, u.Scheme)
	}
	return nil
}

// normalizeIssuer trims a trailing slash so issuer comparisons and
// well-known URL construction are stable regardless of whether the
// operator configured "https://id.example.com" or
// "https://id.example.com/".
func normalizeIssuer(issuer string) string {
	return strings.TrimRight(strings.TrimSpace(issuer), "/")
}
