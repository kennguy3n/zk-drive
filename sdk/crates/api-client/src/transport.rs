//! Shared HTTP transport: base URL, bearer token, default headers.

use std::time::Duration;

use reqwest::header::{HeaderMap, HeaderValue, AUTHORIZATION, USER_AGENT};
use url::Url;

use crate::error::{ApiError, Result};

/// A bearer token. The newtype exists so call sites can't accidentally
/// swap it with another string-typed value (e.g. a workspace id).
#[derive(Debug, Clone)]
pub struct Bearer(pub String);

impl Bearer {
    fn as_header(&self) -> Result<HeaderValue> {
        let raw = format!("Bearer {}", self.0);
        HeaderValue::from_str(&raw).map_err(|e| ApiError::Decode(format!("auth header: {e}")))
    }
}

/// The shared HTTP client. Cheap to clone (wraps a `reqwest::Client`
/// which is itself an `Arc` internally).
#[derive(Debug, Clone)]
pub struct Client {
    pub(crate) http: reqwest::Client,
    pub(crate) base: Url,
}

impl Client {
    pub fn builder(base_url: &str) -> ClientBuilder {
        ClientBuilder {
            base_url: base_url.to_string(),
            bearer: None,
            user_agent: format!("zk-sync-sdk/{}", env!("CARGO_PKG_VERSION")),
            request_timeout: Duration::from_secs(30),
        }
    }

    pub fn base(&self) -> &Url {
        &self.base
    }
}

/// Builder for [`Client`].
pub struct ClientBuilder {
    base_url: String,
    bearer: Option<Bearer>,
    user_agent: String,
    request_timeout: Duration,
}

impl ClientBuilder {
    pub fn bearer(mut self, b: Bearer) -> Self {
        self.bearer = Some(b);
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
        let base = Url::parse(&self.base_url)?;
        let mut headers = HeaderMap::new();
        headers.insert(
            USER_AGENT,
            HeaderValue::from_str(&self.user_agent)
                .map_err(|e| ApiError::Decode(format!("user-agent: {e}")))?,
        );
        if let Some(b) = &self.bearer {
            headers.insert(AUTHORIZATION, b.as_header()?);
        }
        let http = reqwest::Client::builder()
            .default_headers(headers)
            .timeout(self.request_timeout)
            .build()?;
        Ok(Client { http, base })
    }
}

/// Helper for constructing a path-joined URL safely. Strips any
/// leading `/` from `path` so callers can write `"v1/foo"` or
/// `"/v1/foo"` interchangeably.
pub(crate) fn join(base: &Url, path: &str) -> Result<Url> {
    let trimmed = path.trim_start_matches('/');
    base.join(trimmed).map_err(Into::into)
}
