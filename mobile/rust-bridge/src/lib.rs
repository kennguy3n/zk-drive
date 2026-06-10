//! # ZK Drive mobile FFI bridge
//!
//! A single [UniFFI](https://mozilla.github.io/uniffi-rs/) crate that
//! exposes the ZK Drive Rust SDK to the native iOS (Swift / SwiftUI) and
//! Android (Kotlin / Jetpack Compose) apps. One proc-macro source of
//! truth here generates BOTH the Swift and the Kotlin bindings (see
//! `build-ios.sh` / `build-android.sh`), so the two platforms consume a
//! byte-for-byte identical contract.
//!
//! ## What it bridges
//!
//! | Module | Backing SDK crate | Surface |
//! |--------|-------------------|---------|
//! | [`crypto`] | `zk-sync-crypto` | [`CryptoEngine`] — XChaCha20-Poly1305 `encrypt` / `decrypt` |
//! | [`auth`]   | `zk-sync-auth`   | [`TokenManager`] — OAuth2 token storage + transparent refresh |
//! | [`api`]    | `zk-sync-api`    | [`ApiClient`] — presigned upload/download/preview URLs, changefeed |
//! | [`sync`]   | `zk-sync-engine` | [`SyncEngine`] — local SQLite catalogue + changefeed polling |
//!
//! ## Threading contract
//!
//! Crypto is CPU-bound and returns inline. Every method that does
//! network or disk I/O **blocks the calling thread** while driving the
//! work on a shared internal Tokio runtime, so native callers MUST
//! invoke them off the UI thread (iOS `Task.detached` / a background
//! `DispatchQueue`; Android `Dispatchers.IO`). The one exception is
//! [`SyncEngine::start`], which spawns its own background loop and calls
//! back via the foreign [`ChangeObserver`].
//!
//! ## Error model
//!
//! Every fallible call returns [`BridgeError`], a flat typed enum the
//! native layers branch on (retryable [`BridgeError::Network`] vs.
//! permanent [`BridgeError::Auth`] vs. caller-bug
//! [`BridgeError::InvalidInput`], plus HTTP [`BridgeError::Api`] with the
//! status code).

mod api;
mod auth;
mod crypto;
mod error;
mod runtime;
mod sync;

pub use api::{
    ApiClient, ChangePage, ChangeRecord, DownloadTarget, PreviewTarget, UploadTarget,
};
pub use auth::{TokenBundle, TokenManager};
pub use crypto::{generate_dek, CryptoEngine};
pub use error::BridgeError;
pub use sync::{ChangeObserver, FileEntry, SyncEngine, SyncStatus};

uniffi::setup_scaffolding!();

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn crypto_round_trips_through_the_bridge() {
        let dek = generate_dek();
        assert_eq!(dek.len(), 32);
        let engine = CryptoEngine::new(dek).expect("engine");
        let plaintext = b"strict-zk payload crossing the FFI".to_vec();
        let ciphertext = engine.encrypt(plaintext.clone()).expect("encrypt");
        assert_ne!(ciphertext, plaintext, "ciphertext must differ from plaintext");
        let recovered = engine.decrypt(ciphertext).expect("decrypt");
        assert_eq!(recovered, plaintext);
    }

    #[test]
    fn crypto_object_context_binds_aad() {
        let dek = generate_dek();
        let sealing = CryptoEngine::with_object_context(
            dek.clone(),
            "tenant-1".into(),
            "bucket-a".into(),
            "abc123".into(),
            "v1".into(),
            false,
        )
        .expect("sealing engine");
        let ct = sealing.encrypt(b"context-bound".to_vec()).expect("encrypt");

        // Opening with a DIFFERENT object context must fail the AEAD tag
        // check — the AAD is part of the authenticated envelope.
        let wrong = CryptoEngine::with_object_context(
            dek,
            "tenant-1".into(),
            "bucket-a".into(),
            "abc123".into(),
            "v2".into(), // different version_id
            false,
        )
        .expect("wrong engine");
        assert!(wrong.decrypt(ct).is_err());
    }

    #[test]
    fn crypto_rejects_wrong_key_size() {
        // `CryptoEngine`/`TokenManager` are uniffi objects that don't
        // derive Debug, so `unwrap_err()` (which needs `Ok: Debug`) won't
        // compile — match the error out explicitly instead.
        let err = match CryptoEngine::new(vec![0u8; 16]) {
            Ok(_) => panic!("expected a wrong-key-size error"),
            Err(e) => e,
        };
        assert!(matches!(err, BridgeError::InvalidInput(_)));
    }

    #[test]
    fn token_manager_rejects_bad_url() {
        let err = match TokenManager::new("client".into(), "not a url".into()) {
            Ok(_) => panic!("expected a bad-url error"),
            Err(e) => e,
        };
        assert!(matches!(err, BridgeError::InvalidInput(_)));
    }

    #[test]
    fn token_manager_set_get_clear() {
        let tm = TokenManager::new(
            "mobile-client".into(),
            "https://api.zkdrive.test/api/auth/oauth/token".into(),
        )
        .expect("token manager");
        assert!(!tm.has_tokens());
        assert!(tm.snapshot().expect("snapshot").is_none());

        tm.set_tokens(TokenBundle {
            access_token: "at".into(),
            refresh_token: "rt".into(),
            // far future so access_token() returns it without a refresh
            expires_at_unix: 4_102_444_800, // 2100-01-01
            scope: "drive.read drive.write".into(),
        })
        .expect("set");
        assert!(tm.has_tokens());
        assert_eq!(tm.access_token().expect("access"), "at");
        let snap = tm.snapshot().expect("snapshot").expect("some");
        assert_eq!(snap.refresh_token, "rt");
        assert_eq!(snap.scope, "drive.read drive.write");

        tm.clear().expect("clear");
        assert!(!tm.has_tokens());
    }

    #[test]
    fn sync_engine_round_trips_catalogue_rows() {
        let tmp = std::env::temp_dir().join(format!("zk-bridge-{}.db", uuid::Uuid::new_v4()));
        let workspace = uuid::Uuid::new_v4().to_string();
        let tm = TokenManager::new(
            "c".into(),
            "https://api.zkdrive.test/api/auth/oauth/token".into(),
        )
        .expect("tm");
        let api = ApiClient::new("https://api.zkdrive.test".into(), tm).expect("api");
        let engine =
            SyncEngine::new(tmp.to_string_lossy().into_owned(), workspace.clone(), api)
                .expect("engine");

        assert_eq!(engine.workspace_id(), workspace);
        assert_eq!(engine.cursor().expect("cursor"), 0);

        let file_id = uuid::Uuid::new_v4().to_string();
        engine
            .upsert(FileEntry {
                remote_file_id: file_id.clone(),
                remote_version_id: uuid::Uuid::new_v4().to_string(),
                local_path: "/Documents/report.pdf".into(),
                size_bytes: 2048,
                content_hash_hex: "00".repeat(32),
                status: SyncStatus::RemoteDirty,
                pinned: false,
                updated_at_unix_ms: 1_700_000_000_000,
            })
            .expect("upsert");

        let got = engine.get(file_id.clone()).expect("get").expect("some");
        assert_eq!(got.local_path, "/Documents/report.pdf");
        assert_eq!(got.size_bytes, 2048);
        assert_eq!(got.status, SyncStatus::RemoteDirty);

        let pending = engine.pending_downloads().expect("pending");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].remote_file_id, file_id);

        // Commit cursor forward, then reject a regress.
        engine.commit_cursor(42).expect("commit");
        assert_eq!(engine.cursor().expect("cursor"), 42);
        assert!(engine.commit_cursor(7).is_err());

        let _ = std::fs::remove_file(&tmp);
    }

    #[test]
    fn sync_engine_rejects_bad_hash_hex() {
        let tmp = std::env::temp_dir().join(format!("zk-bridge-{}.db", uuid::Uuid::new_v4()));
        let tm = TokenManager::new(
            "c".into(),
            "https://api.zkdrive.test/api/auth/oauth/token".into(),
        )
        .expect("tm");
        let api = ApiClient::new("https://api.zkdrive.test".into(), tm).expect("api");
        let engine = SyncEngine::new(
            tmp.to_string_lossy().into_owned(),
            uuid::Uuid::new_v4().to_string(),
            api,
        )
        .expect("engine");

        let err = engine
            .upsert(FileEntry {
                remote_file_id: uuid::Uuid::new_v4().to_string(),
                remote_version_id: uuid::Uuid::new_v4().to_string(),
                local_path: "/x".into(),
                size_bytes: 0,
                content_hash_hex: "zz".into(), // not valid hex / wrong length
                status: SyncStatus::UpToDate,
                pinned: false,
                updated_at_unix_ms: 0,
            })
            .unwrap_err();
        assert!(matches!(err, BridgeError::InvalidInput(_)));
        let _ = std::fs::remove_file(&tmp);
    }
}
