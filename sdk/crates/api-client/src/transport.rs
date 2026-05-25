//! Shared HTTP transport: base URL, swappable token provider, default headers.
//!
//! The bearer Authorization header is **not** baked into
//! [`reqwest::Client::default_headers`] because the OAuth2 access
//! token rotates: the [`crate::Client`] consults its
//! [`TokenProvider`] on every request, so a freshly-refreshed token
//! is picked up immediately by both HTTP and WebSocket call sites.
//! See [`Client::request`].

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use reqwest::header::{HeaderMap, HeaderValue, AUTHORIZATION, USER_AGENT};
use reqwest::Method;
use url::Url;

use crate::error::{ApiError, Result};

/// A bearer token. The newtype exists so call sites can't accidentally
/// swap it with another string-typed value (e.g. a workspace id).
#[derive(Debug, Clone)]
pub struct Bearer(pub String);

/// Source of access tokens for the HTTP transport. Implementors must
/// be cheap to `clone()` and safe to call from multiple tasks.
///
/// The [`crate::Client`] calls [`TokenProvider::access_token`] on
/// every authenticated request, so the auth crate's coalescing
/// `TokenSource` (which transparently refreshes expired tokens) can
/// be wired in directly.
#[async_trait]
pub trait TokenProvider: Send + Sync + 'static {
    async fn access_token(&self) -> Result<String>;
}

/// Static-token provider — wraps a [`Bearer`] for sessions where
/// token refresh is not yet wired up (e.g. unit tests, the CLI's
/// `--bearer` flag).
#[derive(Debug, Clone)]
pub struct StaticBearer(pub Bearer);

#[async_trait]
impl TokenProvider for StaticBearer {
    async fn access_token(&self) -> Result<String> {
        Ok(self.0 .0.clone())
    }
}

/// The shared HTTP client. Cheap to clone (wraps a `reqwest::Client`
/// which is itself an `Arc` internally).
#[derive(Clone)]
pub struct Client {
    pub(crate) http: reqwest::Client,
    pub(crate) base: Url,
    pub(crate) token: Option<Arc<dyn TokenProvider>>,
}

impl std::fmt::Debug for Client {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Client")
            .field("base", &self.base)
            .field("has_token_provider", &self.token.is_some())
            .finish()
    }
}

impl Client {
    pub fn builder(base_url: &str) -> ClientBuilder {
        ClientBuilder {
            base_url: base_url.to_string(),
            token: None,
            user_agent: format!("zk-sync-sdk/{}", env!("CARGO_PKG_VERSION")),
            request_timeout: Duration::from_secs(30),
        }
    }

    pub fn base(&self) -> &Url {
        &self.base
    }

    /// Construct an authenticated [`reqwest::RequestBuilder`]. The
    /// Authorization header is populated from the configured
    /// [`TokenProvider`] on every call, so freshly-refreshed tokens
    /// are picked up without re-creating the client.
    pub async fn request(&self, method: Method, url: Url) -> Result<reqwest::RequestBuilder> {
        let mut rb = self.http.request(method, url);
        if let Some(tp) = &self.token {
            let token = tp.access_token().await?;
            let raw = format!("Bearer {token}");
            let hv = HeaderValue::from_str(&raw)
                .map_err(|e| ApiError::Decode(format!("auth header: {e}")))?;
            rb = rb.header(AUTHORIZATION, hv);
        }
        Ok(rb)
    }

    /// Fetch the current access token (if any). Useful for the
    /// WebSocket handshake where reqwest is not in play.
    pub async fn access_token(&self) -> Result<Option<String>> {
        match &self.token {
            Some(tp) => Ok(Some(tp.access_token().await?)),
            None => Ok(None),
        }
    }
}

/// Builder for [`Client`].
pub struct ClientBuilder {
    base_url: String,
    token: Option<Arc<dyn TokenProvider>>,
    user_agent: String,
    request_timeout: Duration,
}

impl ClientBuilder {
    /// Configure a static bearer token. Equivalent to
    /// `token_provider(Arc::new(StaticBearer(Bearer(...))))`.
    pub fn bearer(mut self, b: Bearer) -> Self {
        self.token = Some(Arc::new(StaticBearer(b)));
        self
    }

    /// Configure a refreshing token provider — typically the auth
    /// crate's `TokenSource`, adapted to [`TokenProvider`].
    pub fn token_provider(mut self, tp: Arc<dyn TokenProvider>) -> Self {
        self.token = Some(tp);
        self
    }

    pub fn user_agent(mut self, ua: impl Into<String>) -> Self {
        self.user_agent = ua.into();
        self
    }

    pub fn request_timeout(mut self, d: Duration) -> Self {
        self.request_timeout = d;
        self
    }

    pub fn build(self) -> Result<Client> {
        // Normalise the base URL to end with `/` so `Url::join` always
        // *appends* a path segment rather than replacing the last
        // segment of the base. Without this, a base of
        // `https://drive.example.com/api` would silently lose `/api`
        // on every join.
        let mut base_str = self.base_url;
        if !base_str.ends_with('/') {
            base_str.push('/');
        }
        let base = Url::parse(&base_str)?;
        let mut headers = HeaderMap::new();
        headers.insert(
            USER_AGENT,
            HeaderValue::from_str(&self.user_agent)
                .map_err(|e| ApiError::Decode(format!("user-agent: {e}")))?,
        );
        let http = reqwest::Client::builder()
            .default_headers(headers)
            .timeout(self.request_timeout)
            .build()?;
        Ok(Client {
            http,
            base,
            token: self.token,
        })
    }
}

/// Helper for constructing a path-joined URL safely. Strips any
/// leading `/` from `path` so callers can write `"v1/foo"` or
/// `"/v1/foo"` interchangeably. The base URL is guaranteed to end in
/// `/` (see [`ClientBuilder::build`]), so `Url::join` always appends.
pub(crate) fn join(base: &Url, path: &str) -> Result<Url> {
    let trimmed = path.trim_start_matches('/');
    base.join(trimmed).map_err(Into::into)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn build_appends_trailing_slash_to_base() {
        let c = Client::builder("https://example.com/api/v1")
            .build()
            .unwrap();
        assert_eq!(c.base().as_str(), "https://example.com/api/v1/");
        // And join now appends, not replaces.
        let u = join(c.base(), "workspaces/abc/changes").unwrap();
        assert_eq!(
            u.as_str(),
            "https://example.com/api/v1/workspaces/abc/changes"
        );
    }

    #[test]
    fn build_leaves_existing_trailing_slash_alone() {
        let c = Client::builder("https://example.com/").build().unwrap();
        assert_eq!(c.base().as_str(), "https://example.com/");
    }

    #[tokio::test]
    async fn access_token_uses_provider_each_call() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        struct Counting(AtomicUsize);
        #[async_trait]
        impl TokenProvider for Counting {
            async fn access_token(&self) -> Result<String> {
                let n = self.0.fetch_add(1, Ordering::SeqCst);
                Ok(format!("token-{n}"))
            }
        }
        let tp: Arc<dyn TokenProvider> = Arc::new(Counting(AtomicUsize::new(0)));
        let c = Client::builder("https://example.com")
            .token_provider(tp)
            .build()
            .unwrap();
        assert_eq!(c.access_token().await.unwrap().unwrap(), "token-0");
        assert_eq!(c.access_token().await.unwrap().unwrap(), "token-1");
    }
}
