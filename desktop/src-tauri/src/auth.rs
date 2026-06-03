//! OAuth2 PKCE login for the desktop client (RFC 7636 + RFC 8252).
//!
//! The native-app login follows the loopback-interface pattern from
//! RFC 8252 §7.3:
//!
//!   1. Generate a fresh PKCE verifier/challenge via the auth crate's
//!      [`PkceChallenge`].
//!   2. Bind an ephemeral `127.0.0.1` listener to receive the
//!      authorization-code redirect.
//!   3. Open the system browser at
//!      `{base}/api/auth/oauth/{provider}` with the loopback
//!      `redirect_uri`, the S256 `code_challenge`, and an anti-CSRF
//!      `state`.
//!   4. Capture the `?code=…&state=…` redirect on the loopback,
//!      verify `state`, and exchange the code for a [`TokenSet`] at
//!      the backend token endpoint, sending the original
//!      `code_verifier`.
//!   5. Persist the [`TokenSet`] in the OS keychain via the auth
//!      crate's [`KeychainStore`].
//!
//! Tokens are subsequently vended (and transparently refreshed) by
//! the auth crate's [`TokenSource`]; [`build_client`] wires that into
//! the api-client [`Client`] through a [`TokenProvider`] adapter so
//! the sync engine always sends a live bearer.
//!
//! NB (repo deviation): the backend's current
//! `api/auth/oauth/{provider}` handlers implement the *web* variant
//! of this flow — the server mints its own PKCE verifier, stashes it
//! in an `HttpOnly` cookie, and the callback issues a session on the
//! browser origin (see `api/auth/oauth.go`). A production native
//! client needs the backend to additionally honour a loopback
//! `redirect_uri` and return the token bundle to it. This module
//! implements the client half of that native flow; see
//! `desktop/README.md` for the small backend addition it expects.

use std::sync::Arc;
use std::time::Duration;

use chrono::{Duration as ChronoDuration, Utc};
use serde::Deserialize;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use url::Url;
use zk_sync_api::{ApiError, Client, TokenProvider};
use zk_sync_auth::{KeychainStore, PkceChallenge, TokenSet, TokenStore};

use crate::error::DesktopError;

/// Keychain service identifier. Keep stable across releases so tokens
/// survive upgrades; a dev build overrides the `user` half via
/// [`keychain_user_for`] so a side-by-side dev install never clobbers
/// the production credential.
pub const KEYCHAIN_SERVICE: &str = "com.zkdrive.desktop";

/// How long the loopback listener waits for the browser redirect
/// before giving up so a user who closes the browser tab isn't left
/// with a task pinned forever.
const CALLBACK_TIMEOUT: Duration = Duration::from_secs(300);

/// The supported identity providers, mirroring the backend's
/// `auth_provider` column (`api/auth/oauth.go`).
#[derive(Debug, Clone, Copy)]
pub enum Provider {
    Google,
    Microsoft,
}

impl Provider {
    pub fn parse(s: &str) -> Result<Self, DesktopError> {
        match s.to_ascii_lowercase().as_str() {
            "google" => Ok(Provider::Google),
            "microsoft" | "ms" => Ok(Provider::Microsoft),
            other => Err(DesktopError::Auth(format!("unknown provider {other:?}"))),
        }
    }

    fn slug(self) -> &'static str {
        match self {
            Provider::Google => "google",
            Provider::Microsoft => "microsoft",
        }
    }
}

/// Keychain entry name for a given backend base URL so dev / staging /
/// prod installs that point at different servers keep independent
/// tokens.
pub fn keychain_user_for(base_url: &str) -> String {
    base_url.trim_end_matches('/').to_string()
}

/// Persisted token store for `base_url`.
pub fn keychain_store(base_url: &str) -> Arc<dyn TokenStore> {
    Arc::new(KeychainStore::new(
        KEYCHAIN_SERVICE,
        keychain_user_for(base_url),
    ))
}

/// Run the full interactive login and persist the resulting tokens.
/// Returns the granted scope string on success.
pub async fn login(base_url: &str, provider: Provider) -> Result<TokenSet, DesktopError> {
    let pkce = PkceChallenge::generate();
    let state = PkceChallenge::generate().verifier; // reuse the CSPRNG for an opaque state nonce

    // Bind the loopback listener first so we know the exact port to
    // advertise as the redirect_uri.
    let listener = TcpListener::bind(("127.0.0.1", 0)).await?;
    let port = listener.local_addr()?.port();
    let redirect_uri = format!("http://127.0.0.1:{port}/callback");

    let authorize_url = build_authorize_url(base_url, provider, &redirect_uri, &pkce, &state)?;

    // Hand the URL to the system browser. `open` shells out to the
    // platform opener (xdg-open / open / start) so the user logs in
    // in their real browser session, never inside the app webview —
    // the RFC 8252 recommendation.
    open::that(authorize_url.as_str())
        .map_err(|e| DesktopError::Auth(format!("failed to open browser: {e}")))?;

    let code = wait_for_callback(listener, &state).await?;
    let token = exchange_code(base_url, provider, &code, &pkce.verifier, &redirect_uri).await?;

    keychain_store(base_url).save(&token).await?;
    Ok(token)
}

/// Whether a persisted token exists for `base_url` (used to render the
/// login vs. dashboard view on launch).
pub async fn is_logged_in(base_url: &str) -> bool {
    matches!(keychain_store(base_url).load().await, Ok(Some(_)))
}

/// Drop the persisted token. Idempotent — the keychain store maps a
/// missing entry to success.
pub async fn logout(base_url: &str) -> Result<(), DesktopError> {
    keychain_store(base_url).clear().await?;
    Ok(())
}

/// Build an api-client [`Client`] whose bearer is vended by a
/// keychain-backed [`TokenProvider`], so the sync engine always sends
/// a live token and 60s-skew refresh happens transparently.
///
/// The auth crate's `TokenSource`/`HttpRefresher` are not re-exported
/// at the crate root (and the `Refresher` trait is private), so this
/// host implements the equivalent load-or-refresh provider directly
/// on top of the exported [`KeychainStore`] + [`TokenSet`]. The
/// refresh request matches the crate's own `HttpRefresher` wire
/// format (`grant_type=refresh_token`).
pub fn build_client(base_url: &str) -> Result<Arc<Client>, DesktopError> {
    let provider: Arc<dyn TokenProvider> = Arc::new(KeychainTokenProvider {
        store: keychain_store(base_url),
        token_url: token_endpoint(base_url),
        client_id: oauth_client_id(),
        http: reqwest::Client::new(),
        inflight: tokio::sync::Mutex::new(()),
    });
    let client = Client::builder(base_url).token_provider(provider).build()?;
    Ok(Arc::new(client))
}

/// [`TokenProvider`] that reads the persisted [`TokenSet`] from the OS
/// keychain on every request and transparently refreshes it within
/// the 60-second skew window. Concurrent callers coalesce on a single
/// in-flight refresh via `inflight`, mirroring the auth crate's
/// `TokenSource`. A token failure surfaces as a 401-shaped
/// [`ApiError`] so the transport treats it as auth, not a decode bug.
struct KeychainTokenProvider {
    store: Arc<dyn TokenStore>,
    token_url: String,
    client_id: String,
    http: reqwest::Client,
    inflight: tokio::sync::Mutex<()>,
}

impl KeychainTokenProvider {
    async fn load(&self) -> zk_sync_api::Result<TokenSet> {
        self.store
            .load()
            .await
            .map_err(unauthorized)?
            .ok_or_else(|| ApiError::Status {
                status: 401,
                body: "no stored token; login required".into(),
            })
    }

    async fn refresh(&self, refresh_token: &str) -> zk_sync_api::Result<TokenSet> {
        #[derive(serde::Serialize)]
        struct Req<'a> {
            grant_type: &'a str,
            refresh_token: &'a str,
            client_id: &'a str,
        }
        let resp = self
            .http
            .post(&self.token_url)
            .form(&Req {
                grant_type: "refresh_token",
                refresh_token,
                client_id: &self.client_id,
            })
            .send()
            .await
            .map_err(ApiError::from)?;
        if !resp.status().is_success() {
            let status = resp.status().as_u16();
            let body = resp.text().await.unwrap_or_default();
            return Err(ApiError::Status { status, body });
        }
        let parsed: TokenResponse = resp.json().await.map_err(ApiError::from)?;
        // A hostile / misconfigured server can return an
        // astronomically large `expires_in`. chrono's `Add` panics on
        // overflow, so guard with `try_seconds` + `checked_add_signed`
        // and surface it as a 401 rather than crashing the task — same
        // discipline as the SDK's `HttpRefresher` (auth/src/token.rs).
        let lifetime = ChronoDuration::try_seconds(parsed.expires_in).ok_or_else(|| {
            ApiError::Status {
                status: 401,
                body: format!("non-representable expires_in: {}", parsed.expires_in),
            }
        })?;
        let expires_at = Utc::now()
            .checked_add_signed(lifetime)
            .ok_or_else(|| ApiError::Status {
                status: 401,
                body: format!("expires_in would overflow expires_at: {}", parsed.expires_in),
            })?;
        let new = TokenSet {
            access_token: parsed.access_token,
            // A refresh response may omit a new refresh token; keep
            // the existing one in that case.
            refresh_token: if parsed.refresh_token.is_empty() {
                refresh_token.to_string()
            } else {
                parsed.refresh_token
            },
            expires_at,
            scope: parsed.scope,
        };
        self.store.save(&new).await.map_err(unauthorized)?;
        Ok(new)
    }
}

#[async_trait::async_trait]
impl TokenProvider for KeychainTokenProvider {
    async fn access_token(&self) -> zk_sync_api::Result<String> {
        let ts = self.load().await?;
        if !ts.is_expired(Utc::now()) {
            return Ok(ts.access_token);
        }
        // Serialise refresh so a burst of requests fires one refresh.
        let _guard = self.inflight.lock().await;
        let ts = self.load().await?;
        if !ts.is_expired(Utc::now()) {
            return Ok(ts.access_token);
        }
        Ok(self.refresh(&ts.refresh_token).await?.access_token)
    }
}

fn unauthorized(e: zk_sync_auth::AuthError) -> ApiError {
    ApiError::Status {
        status: 401,
        body: format!("token store: {e}"),
    }
}

fn build_authorize_url(
    base_url: &str,
    provider: Provider,
    redirect_uri: &str,
    pkce: &PkceChallenge,
    state: &str,
) -> Result<Url, DesktopError> {
    let mut url = Url::parse(&format!(
        "{}/api/auth/oauth/{}",
        base_url.trim_end_matches('/'),
        provider.slug()
    ))
    .map_err(|e| DesktopError::Auth(format!("invalid base url: {e}")))?;
    url.query_pairs_mut()
        .append_pair("redirect_uri", redirect_uri)
        .append_pair("response_type", "code")
        .append_pair("code_challenge", &pkce.challenge)
        .append_pair("code_challenge_method", "S256")
        .append_pair("state", state);
    Ok(url)
}

fn token_endpoint(base_url: &str) -> String {
    format!("{}/api/auth/oauth/token", base_url.trim_end_matches('/'))
}

/// OAuth client id the native app identifies as. Overridable via env
/// for staging builds; the public-client id is not a secret (PKCE
/// protects the exchange).
fn oauth_client_id() -> String {
    std::env::var("ZK_DRIVE_OAUTH_CLIENT_ID").unwrap_or_else(|_| "zk-drive-desktop".to_string())
}

/// Accept exactly one loopback connection, parse the redirect query,
/// verify `state`, and return the authorization `code`.
async fn wait_for_callback(
    listener: TcpListener,
    expected_state: &str,
) -> Result<String, DesktopError> {
    let accept = async {
        loop {
            let (mut stream, _) = listener.accept().await?;
            let mut buf = vec![0u8; 8192];
            let n = stream.read(&mut buf).await?;
            let request = String::from_utf8_lossy(&buf[..n]);
            let Some(target) = request.lines().next().and_then(parse_request_target) else {
                write_response(&mut stream, "Invalid request").await;
                continue;
            };
            // Ignore favicon / unrelated probes; only the /callback
            // path carries the authorization code.
            if !target.path().ends_with("/callback") {
                write_response(&mut stream, "Waiting for ZK Drive login…").await;
                continue;
            }
            let params: std::collections::HashMap<_, _> =
                target.query_pairs().into_owned().collect();
            if let Some(err) = params.get("error") {
                write_response(&mut stream, "Login failed. You can close this window.").await;
                return Err(DesktopError::Auth(format!(
                    "provider returned error: {err}"
                )));
            }
            match (params.get("state"), params.get("code")) {
                (Some(state), Some(code)) if state == expected_state => {
                    write_response(
                        &mut stream,
                        "ZK Drive login complete — you can close this window and return to the app.",
                    )
                    .await;
                    return Ok(code.clone());
                }
                (Some(_), _) => {
                    write_response(&mut stream, "Login state mismatch.").await;
                    return Err(DesktopError::Auth("state mismatch on callback".into()));
                }
                _ => {
                    write_response(&mut stream, "Missing code.").await;
                    return Err(DesktopError::Auth("callback missing code".into()));
                }
            }
        }
    };

    // `accept` borrows `expected_state`, so await the timeout in place
    // rather than spawning a `'static` task.
    match tokio::time::timeout(CALLBACK_TIMEOUT, accept).await {
        Ok(result) => result,
        Err(_elapsed) => Err(DesktopError::Auth("login timed out".into())),
    }
}

/// Parse the request-target (the middle token of `GET <target>
/// HTTP/1.1`) into an absolute URL rooted at the loopback host so we
/// can use `url`'s query parser.
fn parse_request_target(request_line: &str) -> Option<Url> {
    let target = request_line.split_whitespace().nth(1)?;
    Url::parse(&format!("http://127.0.0.1{target}")).ok()
}

/// Build the full HTTP/1.1 200 response (headers + body) for the
/// loopback page. Kept pure (no I/O) so the framing — in particular
/// that `Content-Length` equals the exact body byte count — is unit
/// testable. Computing the length from the assembled body can't drift
/// the way a hardcoded wrapper-size constant would.
fn render_callback_response(message: &str) -> String {
    let body = format!(
        "<!doctype html><html><body style=\"font-family:system-ui;padding:3rem;text-align:center\"><h2>{message}</h2></body></html>"
    );
    format!(
        "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )
}

async fn write_response(stream: &mut tokio::net::TcpStream, message: &str) {
    let payload = render_callback_response(message);
    let _ = stream.write_all(payload.as_bytes()).await;
    let _ = stream.flush().await;
}

/// Token-endpoint response shape (the common OAuth2 token response).
#[derive(Debug, Deserialize)]
struct TokenResponse {
    access_token: String,
    #[serde(default)]
    refresh_token: String,
    expires_in: i64,
    #[serde(default)]
    scope: String,
}

/// Exchange the authorization code for a [`TokenSet`], sending the
/// PKCE `code_verifier` so the server can recompute the challenge.
async fn exchange_code(
    base_url: &str,
    _provider: Provider,
    code: &str,
    verifier: &str,
    redirect_uri: &str,
) -> Result<TokenSet, DesktopError> {
    #[derive(serde::Serialize)]
    struct Form<'a> {
        grant_type: &'a str,
        code: &'a str,
        code_verifier: &'a str,
        redirect_uri: &'a str,
        client_id: &'a str,
    }
    let http = reqwest::Client::new();
    let client_id = oauth_client_id();
    let resp = http
        .post(token_endpoint(base_url))
        .form(&Form {
            grant_type: "authorization_code",
            code,
            code_verifier: verifier,
            redirect_uri,
            client_id: &client_id,
        })
        .send()
        .await?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(DesktopError::Auth(format!(
            "token exchange failed ({}): {body}",
            status.as_u16()
        )));
    }
    let parsed: TokenResponse = resp
        .json()
        .await
        .map_err(|e| DesktopError::Auth(format!("decode token response: {e}")))?;

    // Guard against a non-representable / overflowing `expires_in` so a
    // bad token response is a typed error, not a panic (see `refresh`).
    let lifetime = ChronoDuration::try_seconds(parsed.expires_in).ok_or_else(|| {
        DesktopError::Auth(format!(
            "non-representable expires_in: {}",
            parsed.expires_in
        ))
    })?;
    let expires_at = Utc::now()
        .checked_add_signed(lifetime)
        .ok_or_else(|| DesktopError::Auth("expires_in would overflow expires_at".into()))?;
    Ok(TokenSet {
        access_token: parsed.access_token,
        refresh_token: parsed.refresh_token,
        expires_at,
        scope: parsed.scope,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Split a raw HTTP/1.1 response into (headers, body) on the
    /// blank-line terminator.
    fn split_response(raw: &str) -> (&str, &str) {
        raw.split_once("\r\n\r\n")
            .expect("response must have a header/body separator")
    }

    fn header_value<'a>(headers: &'a str, name: &str) -> Option<&'a str> {
        headers.lines().find_map(|line| {
            let (k, v) = line.split_once(':')?;
            k.trim()
                .eq_ignore_ascii_case(name)
                .then(|| v.trim())
        })
    }

    #[test]
    fn callback_response_content_length_matches_body_bytes() {
        // The Content-Length must equal the exact byte count of the
        // body — regression for the off-by-9 hardcoded wrapper size.
        for message in ["Login complete — you can close this window.", "Invalid request", ""] {
            let raw = render_callback_response(message);
            let (headers, body) = split_response(&raw);
            let declared: usize = header_value(headers, "Content-Length")
                .expect("Content-Length header present")
                .parse()
                .expect("Content-Length is a number");
            assert_eq!(
                declared,
                body.len(),
                "declared Content-Length must equal body byte length for {message:?}"
            );
            assert!(body.contains(message), "body should embed the message");
        }
    }

    #[test]
    fn parse_request_target_extracts_query() {
        let url = parse_request_target("GET /callback?code=abc123&state=xyz HTTP/1.1")
            .expect("valid request target parses");
        assert_eq!(url.path(), "/callback");
        let pairs: std::collections::HashMap<_, _> = url.query_pairs().into_owned().collect();
        assert_eq!(pairs.get("code").map(String::as_str), Some("abc123"));
        assert_eq!(pairs.get("state").map(String::as_str), Some("xyz"));
    }

    #[test]
    fn parse_request_target_rejects_malformed_line() {
        assert!(parse_request_target("garbage-without-a-target").is_none());
    }

    #[test]
    fn build_authorize_url_carries_pkce_and_state() {
        let pkce = PkceChallenge::generate();
        let url = build_authorize_url(
            "https://drive.example.com/",
            Provider::Google,
            "http://127.0.0.1:51789/callback",
            &pkce,
            "state-token",
        )
        .expect("authorize url builds");

        assert_eq!(url.path(), "/api/auth/oauth/google");
        let pairs: std::collections::HashMap<_, _> = url.query_pairs().into_owned().collect();
        assert_eq!(pairs.get("response_type").map(String::as_str), Some("code"));
        assert_eq!(
            pairs.get("code_challenge_method").map(String::as_str),
            Some("S256")
        );
        assert_eq!(
            pairs.get("code_challenge").map(String::as_str),
            Some(pkce.challenge.as_str())
        );
        assert_eq!(pairs.get("state").map(String::as_str), Some("state-token"));
        assert_eq!(
            pairs.get("redirect_uri").map(String::as_str),
            Some("http://127.0.0.1:51789/callback")
        );
    }

    #[test]
    fn microsoft_provider_uses_its_slug() {
        let pkce = PkceChallenge::generate();
        let url = build_authorize_url(
            "https://drive.example.com",
            Provider::Microsoft,
            "http://127.0.0.1:1/callback",
            &pkce,
            "s",
        )
        .expect("authorize url builds");
        assert_eq!(url.path(), "/api/auth/oauth/microsoft");
    }
}
