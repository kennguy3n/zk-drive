//! Token persistence.
//!
//! In production the desktop SDK uses [`KeychainStore`], which writes
//! the serialised [`TokenSet`] to the OS credential store. Tests use
//! [`MemoryStore`].

use std::sync::Mutex;

use async_trait::async_trait;

use crate::token::TokenSet;
use crate::Result;

#[async_trait]
pub trait TokenStore: Send + Sync {
    async fn load(&self) -> Result<Option<TokenSet>>;
    async fn save(&self, t: &TokenSet) -> Result<()>;
    async fn clear(&self) -> Result<()>;
}

/// A `TokenStore` that simply holds the bundle in process memory.
/// Useful for tests and ephemeral cli flows where persistence isn't
/// required. Thread-safe via an internal mutex.
#[derive(Default)]
pub struct MemoryStore {
    inner: Mutex<Option<TokenSet>>,
}

#[async_trait]
impl TokenStore for MemoryStore {
    async fn load(&self) -> Result<Option<TokenSet>> {
        Ok(self.inner.lock().unwrap().clone())
    }

    async fn save(&self, t: &TokenSet) -> Result<()> {
        *self.inner.lock().unwrap() = Some(t.clone());
        Ok(())
    }

    async fn clear(&self) -> Result<()> {
        *self.inner.lock().unwrap() = None;
        Ok(())
    }
}

/// `TokenStore` backed by the OS keychain.
///
/// The `service` and `user` strings determine the keychain entry's
/// service identifier; pick stable values per environment so multiple
/// installs (e.g. dev + prod) don't clobber each other.
pub struct KeychainStore {
    service: String,
    user: String,
}

impl KeychainStore {
    pub fn new(service: impl Into<String>, user: impl Into<String>) -> Self {
        Self {
            service: service.into(),
            user: user.into(),
        }
    }

    fn entry(&self) -> Result<keyring::Entry> {
        keyring::Entry::new(&self.service, &self.user).map_err(Into::into)
    }
}

#[async_trait]
impl TokenStore for KeychainStore {
    async fn load(&self) -> Result<Option<TokenSet>> {
        // keyring is sync; offload to a blocking task so the runtime
        // isn't blocked by a long-running keychain unlock prompt.
        let entry = self.entry()?;
        let raw = tokio::task::spawn_blocking(move || entry.get_password())
            .await
            .map_err(|e| crate::AuthError::OAuth(format!("join: {e}")))?;
        match raw {
            Ok(s) => Ok(Some(serde_json::from_str(&s)?)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    async fn save(&self, t: &TokenSet) -> Result<()> {
        let entry = self.entry()?;
        let payload = serde_json::to_string(t)?;
        tokio::task::spawn_blocking(move || entry.set_password(&payload))
            .await
            .map_err(|e| crate::AuthError::OAuth(format!("join: {e}")))??;
        Ok(())
    }

    async fn clear(&self) -> Result<()> {
        let entry = self.entry()?;
        let result = tokio::task::spawn_blocking(move || entry.delete_credential())
            .await
            .map_err(|e| crate::AuthError::OAuth(format!("join: {e}")))?;
        // Treat "credential is already gone" as success so logout
        // remains idempotent. This mirrors how `load` maps NoEntry to
        // Ok(None) -- a logout call that races a manual keychain
        // deletion (or that runs without an earlier successful save)
        // should not surface as an error to the caller.
        match result {
            Ok(()) => Ok(()),
            Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(crate::AuthError::Keyring(e)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::{Duration, Utc};

    #[tokio::test]
    async fn memory_store_round_trip() {
        let s = MemoryStore::default();
        assert!(s.load().await.unwrap().is_none());
        let ts = TokenSet {
            access_token: "a".into(),
            refresh_token: "r".into(),
            expires_at: Utc::now() + Duration::seconds(60),
            scope: "drive.read".into(),
        };
        s.save(&ts).await.unwrap();
        let loaded = s.load().await.unwrap().unwrap();
        assert_eq!(loaded.access_token, "a");
        s.clear().await.unwrap();
        assert!(s.load().await.unwrap().is_none());
    }
}
