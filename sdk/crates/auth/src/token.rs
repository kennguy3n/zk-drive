//! Token model + refresh flow.

use std::sync::Arc;

use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use tokio::sync::Mutex;

use crate::store::TokenStore;
use crate::{AuthError, Result};

/// One OAuth2 token bundle. The refresh token is held in the same
/// struct so the [`TokenSource`] can transparently rotate access
/// tokens when they expire.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TokenSet {
    pub access_token: String,
    pub refresh_token: String,
    /// Absolute expiry, computed at issuance from
    /// `expires_in` + clock.
    pub expires_at: DateTime<Utc>,
    /// Originally-issued scopes, space-joined.
    pub scope: String,
}

impl TokenSet {
    pub fn is_expired(&self, now: DateTime<Utc>) -> bool {
        // 60-second skew so we proactively refresh before the
        // access token is rejected by an upstream replica. Use
        // `checked_add_signed` to mirror `HttpRefresher::refresh`
        // and stay panic-free if `now` is somehow close to
        // `DateTime::MAX` (e.g. an OS clock that wandered into the
        // far future). Falling back to "expired" is the safe
        // choice: the caller will issue a refresh, which is the
        // worst case here, not a panic.
        match now.checked_add_signed(Duration::seconds(60)) {
            Some(t) => t >= self.expires_at,
            None => true,
        }
    }
}

/// Token vending machine. Concurrent calls to [`TokenSource::access_token`]
/// share a single in-flight refresh so a thundering herd of N tasks
/// doesn't fire N refresh requests.
pub struct TokenSource {
    store: Arc<dyn TokenStore>,
    refresher: Arc<dyn Refresher>,
    inflight: Mutex<()>,
}

#[async_trait::async_trait]
pub trait Refresher: Send + Sync + 'static {
    async fn refresh(&self, refresh_token: &str) -> Result<TokenSet>;
}

impl TokenSource {
    pub fn new(store: Arc<dyn TokenStore>, refresher: Arc<dyn Refresher>) -> Self {
        Self {
            store,
            refresher,
            inflight: Mutex::new(()),
        }
    }

    /// Returns a still-valid access token. If the persisted token is
    /// within the 60-second skew window of expiry it is transparently
    /// refreshed and the new token is written back to the store.
    pub async fn access_token(&self) -> Result<String> {
        let ts = self.store.load().await?.ok_or(AuthError::MissingToken)?;
        if !ts.is_expired(Utc::now()) {
            return Ok(ts.access_token);
        }
        // Serialise refresh attempts so a burst of callers doesn't
        // fan out into N refresh requests.
        let _guard = self.inflight.lock().await;
        // Re-check inside the critical section in case another caller
        // already refreshed while we were waiting.
        let ts = self.store.load().await?.ok_or(AuthError::MissingToken)?;
        if !ts.is_expired(Utc::now()) {
            return Ok(ts.access_token);
        }
        let new = self.refresher.refresh(&ts.refresh_token).await?;
        self.store.save(&new).await?;
        Ok(new.access_token)
    }
}

/// A simple HTTP refresher pointed at the ZK Drive `/oauth/token`
/// endpoint. Other refreshers (test fixtures, alternative IdPs) can
/// implement [`Refresher`] directly.
///
/// The struct itself is constructed by the CLI / Tauri shell when
/// they wire up the [`TokenSource`]. Held as `pub` so callers can
/// hand-build it for staging environments where the token URL is
/// not the production one.
#[allow(dead_code)]
pub struct HttpRefresher {
    pub client_id: String,
    pub token_url: String,
    pub http: reqwest::Client,
}

#[async_trait::async_trait]
impl Refresher for HttpRefresher {
    async fn refresh(&self, refresh_token: &str) -> Result<TokenSet> {
        #[derive(Serialize)]
        struct Req<'a> {
            grant_type: &'a str,
            refresh_token: &'a str,
            client_id: &'a str,
        }
        #[derive(Deserialize)]
        struct Resp {
            access_token: String,
            refresh_token: Option<String>,
            expires_in: i64,
            #[serde(default)]
            scope: Option<String>,
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
            .await?;
        if !resp.status().is_success() {
            let s = resp.status().as_u16();
            let body = resp.text().await.unwrap_or_default();
            return Err(AuthError::OAuth(format!("refresh status {s}: {body}")));
        }
        let r: Resp = resp
            .json()
            .await
            .map_err(|e| AuthError::Decode(format!("{e}")))?;
        // A hostile / misconfigured server can return an
        // astronomically large `expires_in`. chrono's `Add` panics on
        // overflow, so guard the computation with `try_seconds` +
        // `checked_add_signed` and surface the bad response as a
        // typed error instead of crashing the refresher task.
        let lifetime = Duration::try_seconds(r.expires_in).ok_or_else(|| {
            AuthError::OAuth(format!(
                "refresh response carries non-representable expires_in: {}",
                r.expires_in
            ))
        })?;
        let expires_at = Utc::now().checked_add_signed(lifetime).ok_or_else(|| {
            AuthError::OAuth(format!(
                "refresh response would overflow expires_at: expires_in={}",
                r.expires_in
            ))
        })?;
        Ok(TokenSet {
            access_token: r.access_token,
            refresh_token: r.refresh_token.unwrap_or_else(|| refresh_token.to_string()),
            expires_at,
            scope: r.scope.unwrap_or_default(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::MemoryStore;
    use std::sync::atomic::{AtomicUsize, Ordering};

    struct CountingRefresher(AtomicUsize);

    #[async_trait::async_trait]
    impl Refresher for CountingRefresher {
        async fn refresh(&self, _: &str) -> Result<TokenSet> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Ok(TokenSet {
                access_token: "fresh".into(),
                refresh_token: "rt".into(),
                expires_at: Utc::now() + Duration::seconds(3600),
                scope: "drive.read".into(),
            })
        }
    }

    #[tokio::test]
    async fn returns_cached_token_when_not_expired() {
        let store = Arc::new(MemoryStore::default());
        store
            .save(&TokenSet {
                access_token: "cached".into(),
                refresh_token: "rt".into(),
                expires_at: Utc::now() + Duration::seconds(600),
                scope: "drive.read".into(),
            })
            .await
            .unwrap();
        let ts = TokenSource::new(store, Arc::new(CountingRefresher(AtomicUsize::new(0))));
        let tok = ts.access_token().await.unwrap();
        assert_eq!(tok, "cached");
    }

    #[tokio::test]
    async fn refreshes_when_expired_and_persists_new_token() {
        let store: Arc<dyn TokenStore> = Arc::new(MemoryStore::default());
        store
            .save(&TokenSet {
                access_token: "stale".into(),
                refresh_token: "rt".into(),
                expires_at: Utc::now() - Duration::seconds(1),
                scope: "drive.read".into(),
            })
            .await
            .unwrap();
        let counter = Arc::new(CountingRefresher(AtomicUsize::new(0)));
        let ts = TokenSource::new(store.clone(), counter.clone());
        let tok = ts.access_token().await.unwrap();
        assert_eq!(tok, "fresh");
        // The new token should be on disk.
        let persisted = store.load().await.unwrap().unwrap();
        assert_eq!(persisted.access_token, "fresh");
        // Subsequent calls within validity must not refresh again.
        let tok2 = ts.access_token().await.unwrap();
        assert_eq!(tok2, "fresh");
        assert_eq!(counter.0.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn concurrent_callers_share_single_refresh() {
        let store: Arc<dyn TokenStore> = Arc::new(MemoryStore::default());
        store
            .save(&TokenSet {
                access_token: "stale".into(),
                refresh_token: "rt".into(),
                expires_at: Utc::now() - Duration::seconds(10),
                scope: "drive.read".into(),
            })
            .await
            .unwrap();
        let counter = Arc::new(CountingRefresher(AtomicUsize::new(0)));
        let ts = Arc::new(TokenSource::new(store, counter.clone()));

        let mut handles = Vec::new();
        for _ in 0..8 {
            let ts = ts.clone();
            handles.push(tokio::spawn(async move { ts.access_token().await }));
        }
        for h in handles {
            assert_eq!(h.await.unwrap().unwrap(), "fresh");
        }
        // Single refresh despite 8 concurrent demands.
        assert_eq!(counter.0.load(Ordering::SeqCst), 1);
    }
}
