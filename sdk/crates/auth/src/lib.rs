//! Authentication for the ZK Drive desktop SDK.
//!
//! The desktop agent uses OAuth2 PKCE (RFC 7636) against the ZK
//! Drive backend's `/oauth/authorize` and `/oauth/token` endpoints.
//! Tokens are stored in the OS keychain (Apple Keychain, libsecret,
//! Windows Credential Manager) via the [`keyring`] crate so they
//! survive process restarts and aren't readable by other unprivileged
//! processes on the same host.

mod pkce;
mod store;
mod token;

pub use pkce::PkceChallenge;
pub use store::{KeychainStore, MemoryStore, TokenStore};
pub use token::{HttpRefresher, Refresher, TokenSet, TokenSource};

use thiserror::Error;

#[derive(Debug, Error)]
pub enum AuthError {
    #[error("auth: missing token (call login() first)")]
    MissingToken,
    #[error("auth: keyring: {0}")]
    Keyring(#[from] keyring::Error),
    #[error("auth: oauth: {0}")]
    OAuth(String),
    #[error("auth: transport: {0}")]
    Transport(#[from] reqwest::Error),
    #[error("auth: decode: {0}")]
    Decode(String),
    #[error("auth: io: {0}")]
    Io(#[from] std::io::Error),
    #[error("auth: serialize: {0}")]
    Serialize(#[from] serde_json::Error),
}

pub type Result<T> = std::result::Result<T, AuthError>;
