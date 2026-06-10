//! The single error type crossing the FFI boundary.
//!
//! UniFFI flattens this into a Swift `enum BridgeError: Error` and a
//! Kotlin `sealed class BridgeException`. Every fallible exported
//! method returns `Result<_, BridgeError>`, so the native layers get
//! a typed, exhaustive error surface instead of opaque strings.
//!
//! The variants are deliberately coarse: native callers branch on the
//! *category* (retryable network blip vs. permanent auth failure vs.
//! caller bug) and surface `message` for diagnostics. Mapping each
//! underlying crate error into the right category happens in the
//! `From` impls below so individual call sites stay free of error
//! plumbing.

use zk_sync_api::ApiError;
use zk_sync_auth::AuthError;
use zk_sync_crypto::Error as CryptoError;
use zk_sync_engine::SyncError;

/// Error returned across the FFI boundary.
#[derive(Debug, thiserror::Error, uniffi::Error)]
#[uniffi(flat_error)]
pub enum BridgeError {
    /// Encryption / decryption failed (bad key length, truncated
    /// ciphertext, authentication-tag mismatch). Not retryable.
    #[error("crypto: {0}")]
    Crypto(String),

    /// Token acquisition / refresh failed, or no token has been set.
    /// The caller should drive the user back through sign-in.
    #[error("auth: {0}")]
    Auth(String),

    /// Transport-level failure reaching the backend (DNS, TLS, timeout,
    /// connection reset). Retryable with backoff.
    #[error("network: {0}")]
    Network(String),

    /// The backend returned a non-2xx HTTP status. `status` is the HTTP
    /// code so callers can distinguish 401 (re-auth), 403 (permission),
    /// 404 (gone) and 5xx (retry).
    #[error("api: status {status}: {message}")]
    Api { status: u16, message: String },

    /// Local SQLite catalogue failure (open, migration, query).
    #[error("catalogue: {0}")]
    Catalogue(String),

    /// The caller passed an argument the bridge rejected before doing
    /// any work (malformed UUID, empty path, wrong key size). Always a
    /// programming error on the native side.
    #[error("invalid input: {0}")]
    InvalidInput(String),
}

impl From<CryptoError> for BridgeError {
    fn from(e: CryptoError) -> Self {
        BridgeError::Crypto(e.to_string())
    }
}

impl From<AuthError> for BridgeError {
    fn from(e: AuthError) -> Self {
        match e {
            // A transport error during refresh is a network problem, not
            // a credential problem — keep it retryable so the native
            // layer doesn't bounce the user to sign-in on a flaky link.
            AuthError::Transport(inner) => BridgeError::Network(inner.to_string()),
            other => BridgeError::Auth(other.to_string()),
        }
    }
}

impl From<ApiError> for BridgeError {
    fn from(e: ApiError) -> Self {
        match e {
            ApiError::Status { status, body } => BridgeError::Api {
                status,
                message: truncate_body(&body),
            },
            // The transport failed to obtain a token from our provider —
            // that is an auth problem, not a transport blip.
            ApiError::Token(msg) => BridgeError::Auth(msg),
            other => BridgeError::Network(other.to_string()),
        }
    }
}

impl From<SyncError> for BridgeError {
    fn from(e: SyncError) -> Self {
        match e {
            SyncError::Sqlite(inner) => BridgeError::Catalogue(inner.to_string()),
            SyncError::Io(inner) => BridgeError::Catalogue(inner.to_string()),
            SyncError::Api(inner) => BridgeError::from(inner),
            SyncError::Crypto(inner) => BridgeError::from(inner),
            SyncError::Auth(inner) => BridgeError::from(inner),
            other => BridgeError::Catalogue(other.to_string()),
        }
    }
}

impl From<url::ParseError> for BridgeError {
    fn from(e: url::ParseError) -> Self {
        BridgeError::InvalidInput(format!("invalid url: {e}"))
    }
}

/// Cap an error body echoed back across the FFI so a hostile or
/// misbehaving server can't push an unbounded string through to the
/// native UI. 512 bytes is plenty for a JSON error envelope; the full
/// body is still logged server-side.
fn truncate_body(body: &str) -> String {
    const MAX: usize = 512;
    if body.len() <= MAX {
        return body.to_string();
    }
    // Respect char boundaries so we never split a multi-byte sequence.
    let mut end = MAX;
    while end > 0 && !body.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &body[..end])
}

/// Bridge-local result alias.
pub type Result<T> = std::result::Result<T, BridgeError>;
