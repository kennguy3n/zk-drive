//! Error type for the api-client crate.

use thiserror::Error;

pub type Result<T> = std::result::Result<T, ApiError>;

#[derive(Debug, Error)]
pub enum ApiError {
    /// The server returned a non-2xx status. The payload (if any) is
    /// captured verbatim so call sites can render the server's error
    /// detail to the user / log without re-parsing.
    #[error("api: status {status}: {body}")]
    Status { status: u16, body: String },

    /// The response body was not the JSON shape we expected for
    /// this endpoint.
    #[error("api: decode response: {0}")]
    Decode(String),

    /// HTTP transport failed (DNS, TLS, timeout, ...).
    #[error("api: transport: {0}")]
    Transport(#[from] reqwest::Error),

    /// WebSocket protocol failure on the change feed stream.
    #[error("api: websocket: {0}")]
    WebSocket(String),

    /// The configured [`crate::TokenProvider`] could not vend an access
    /// token for an authenticated request (e.g. the underlying OAuth
    /// refresh failed or no token has been set). Carries the provider's
    /// error rendered as a string so the transport layer can surface a
    /// faithful reason without depending on the auth crate's error type.
    #[error("api: token: {0}")]
    Token(String),

    /// URL parsing or building failed (only happens for a malformed
    /// base URL configured by the caller).
    #[error("api: url: {0}")]
    Url(#[from] url::ParseError),

    /// JSON serialisation of an outgoing request body failed.
    #[error("api: encode request: {0}")]
    Encode(#[from] serde_json::Error),
}

impl ApiError {
    pub(crate) fn websocket(s: impl Into<String>) -> Self {
        ApiError::WebSocket(s.into())
    }
}
