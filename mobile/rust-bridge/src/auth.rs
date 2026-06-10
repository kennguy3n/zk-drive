//! OAuth2 token management.
//!
//! Wraps [`zk_sync_auth::TokenSource`] so the native app can hand the
//! bridge a token bundle once and then let every authenticated request
//! transparently refresh it. The bridge deliberately uses an in-memory
//! store rather than the desktop SDK's OS-keychain store: on mobile the
//! platform owns secure storage (iOS Keychain via `kSecAttrAccessible…`,
//! Android `EncryptedSharedPreferences` / Keystore), and the generic
//! `keyring` crate has no first-class iOS/Android backend. So the
//! contract is:
//!
//!   1. Native layer persists the [`TokenSet`] in platform-secure
//!      storage after sign-in.
//!   2. On launch it re-seeds the bridge via [`TokenManager::set_tokens`].
//!   3. The bridge refreshes access tokens on demand; the native layer
//!      observes the refreshed values via [`TokenManager::snapshot`] and
//!      writes them back to secure storage.

use std::sync::Arc;

use chrono::{DateTime, TimeZone, Utc};
use zk_sync_api::{ApiError, TokenProvider};
use zk_sync_auth::{MemoryStore, TokenSet, TokenSource, TokenStore};

use crate::error::{BridgeError, Result};
use crate::runtime::block_on;

/// A token bundle crossing the FFI. Mirrors [`zk_sync_auth::TokenSet`]
/// with FFI-friendly field types (`i64` Unix-seconds expiry instead of
/// a chrono type).
#[derive(Debug, Clone, uniffi::Record)]
pub struct TokenBundle {
    pub access_token: String,
    pub refresh_token: String,
    /// Absolute access-token expiry, Unix epoch seconds.
    pub expires_at_unix: i64,
    /// Space-joined granted scopes; empty string when the server did
    /// not echo a scope.
    pub scope: String,
}

impl TokenBundle {
    fn into_token_set(self) -> Result<TokenSet> {
        let expires_at = unix_to_utc(self.expires_at_unix)?;
        Ok(TokenSet {
            access_token: self.access_token,
            refresh_token: self.refresh_token,
            expires_at,
            scope: self.scope,
        })
    }

    fn from_token_set(ts: &TokenSet) -> Self {
        Self {
            access_token: ts.access_token.clone(),
            refresh_token: ts.refresh_token.clone(),
            expires_at_unix: ts.expires_at.timestamp(),
            scope: ts.scope.clone(),
        }
    }
}

/// Manages the signed-in user's OAuth2 tokens and refreshes them
/// against the backend's `/oauth/token` endpoint on demand.
#[derive(uniffi::Object)]
pub struct TokenManager {
    store: Arc<MemoryStore>,
    source: Arc<TokenSource>,
}

#[uniffi::export]
impl TokenManager {
    /// Construct a manager that refreshes tokens for `client_id`
    /// against `token_url` (the backend's absolute `/oauth/token`
    /// endpoint, e.g. `https://api.zkdrive.example.com/api/auth/oauth/token`).
    ///
    /// No token is loaded yet; call [`set_tokens`](Self::set_tokens)
    /// with the bundle from sign-in (or restored from secure storage)
    /// before issuing authenticated requests.
    #[uniffi::constructor]
    pub fn new(client_id: String, token_url: String) -> Result<Arc<Self>> {
        // Validate the URL eagerly so a typo fails at construction
        // rather than on the first silent refresh attempt.
        url::Url::parse(&token_url)?;
        let store = Arc::new(MemoryStore::default());
        let refresher = Arc::new(zk_sync_auth_http_refresher(client_id, token_url));
        let source = Arc::new(TokenSource::new(store.clone(), refresher));
        Ok(Arc::new(Self { store, source }))
    }

    /// Seed (or replace) the stored token bundle. Call after sign-in and
    /// on every launch to restore the persisted tokens.
    pub fn set_tokens(&self, tokens: TokenBundle) -> Result<()> {
        let ts = tokens.into_token_set()?;
        block_on(self.store.save(&ts)).map_err(BridgeError::from)
    }

    /// Return a still-valid access token, refreshing transparently if
    /// the stored one is within the 60-second expiry skew. Blocks on a
    /// network round-trip when a refresh is needed; call off the UI
    /// thread.
    ///
    /// Errors with [`BridgeError::Auth`] when no tokens have been set or
    /// the refresh token has been revoked, and [`BridgeError::Network`]
    /// on a transport failure (retry the latter).
    pub fn access_token(&self) -> Result<String> {
        block_on(self.source.access_token()).map_err(BridgeError::from)
    }

    /// Current stored token bundle, or `None` when no tokens are set.
    /// The native layer reads this after an authenticated call to pick
    /// up a freshly-refreshed access token and persist it.
    pub fn snapshot(&self) -> Result<Option<TokenBundle>> {
        let loaded = block_on(self.store.load()).map_err(BridgeError::from)?;
        Ok(loaded.as_ref().map(TokenBundle::from_token_set))
    }

    /// Whether a token bundle is currently loaded.
    pub fn has_tokens(&self) -> bool {
        matches!(block_on(self.store.load()), Ok(Some(_)))
    }

    /// Drop the stored tokens (sign-out). Idempotent.
    pub fn clear(&self) -> Result<()> {
        block_on(self.store.clear()).map_err(BridgeError::from)
    }
}

impl TokenManager {
    /// Borrow the inner [`TokenSource`] so the API client can use it as a
    /// [`TokenProvider`] that refreshes on every request.
    pub(crate) fn token_provider(&self) -> Arc<dyn TokenProvider> {
        Arc::new(SourceTokenProvider {
            source: self.source.clone(),
        })
    }
}

/// Adapts a [`TokenSource`] to the api-client's [`TokenProvider`] trait
/// so authenticated HTTP requests pull a fresh (auto-refreshed) bearer
/// at send time.
struct SourceTokenProvider {
    source: Arc<TokenSource>,
}

#[async_trait::async_trait]
impl TokenProvider for SourceTokenProvider {
    async fn access_token(&self) -> zk_sync_api::Result<String> {
        // The api-client transport speaks its own error type. Render the
        // auth failure into `ApiError::Token` so the reason survives the
        // round-trip; the bridge maps it back to `BridgeError::Auth`.
        self.source
            .access_token()
            .await
            .map_err(|e| ApiError::Token(e.to_string()))
    }
}

/// Build the SDK's HTTP refresher. Factored out so the (private) field
/// layout of `HttpRefresher` stays an implementation detail of the auth
/// crate and this module only depends on its public constructor shape.
fn zk_sync_auth_http_refresher(
    client_id: String,
    token_url: String,
) -> zk_sync_auth::HttpRefresher {
    zk_sync_auth::HttpRefresher {
        client_id,
        token_url,
        http: reqwest::Client::new(),
    }
}

fn unix_to_utc(secs: i64) -> Result<DateTime<Utc>> {
    match Utc.timestamp_opt(secs, 0).single() {
        Some(dt) => Ok(dt),
        None => Err(BridgeError::InvalidInput(format!(
            "expires_at_unix {secs} is not a representable timestamp"
        ))),
    }
}
