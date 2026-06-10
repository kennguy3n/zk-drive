//! Client-side encryption surface.
//!
//! Wraps [`zk_sync_crypto`] — the Rust mirror of the Go Strict-ZK
//! envelope — so iOS and Android encrypt / decrypt file bytes with the
//! exact same XChaCha20-Poly1305 framing the desktop SDK and the Go
//! backend use. Ciphertext produced here is byte-identical to every
//! other ZK Drive client for the same DEK + AAD, so a file uploaded
//! from the phone downloads cleanly on the desktop and vice versa.

use std::sync::Arc;

use zk_sync_crypto::{decrypt_to_vec, encrypt_to_vec, ChunkAadFormat, DataEncryptionKey, Options};

use crate::error::{BridgeError, Result};

/// Length of a ZK Drive data-encryption key, in bytes.
const DEK_LEN: usize = 32;

/// A crypto context bound to one data-encryption key (DEK) and a fixed
/// AAD / nonce policy.
///
/// The native app obtains the per-file DEK out of band (unwrapped from
/// the file's [`EncryptionEnvelope`] against the workspace master key)
/// and constructs one `CryptoEngine` per file it is transferring. The
/// engine holds the raw key for its lifetime, so the native side
/// should drop it promptly once a transfer completes.
#[derive(uniffi::Object)]
pub struct CryptoEngine {
    dek: DataEncryptionKey,
    convergent_nonce: bool,
    chunk_aad: Vec<u8>,
}

#[uniffi::export]
impl CryptoEngine {
    /// Build an engine for a raw 32-byte DEK with no per-chunk AAD and
    /// random nonces — the legacy-compatible default that matches
    /// objects sealed before object-context AAD existed.
    ///
    /// Returns [`BridgeError::InvalidInput`] if `dek` is not exactly 32
    /// bytes.
    #[uniffi::constructor]
    pub fn new(dek: Vec<u8>) -> Result<Arc<Self>> {
        let dek = parse_dek(&dek)?;
        Ok(Arc::new(Self {
            dek,
            convergent_nonce: false,
            chunk_aad: Vec::new(),
        }))
    }

    /// Build an engine that binds every chunk to its object context via
    /// the canonical `tenant_id|bucket|object_key_hash_hex|version_id`
    /// AAD, matching [`ChunkAadFormat::Canonical`]. This is the form new
    /// uploads should use: it cryptographically pins ciphertext to the
    /// object it belongs to, so a chunk can't be replayed under a
    /// different object key.
    ///
    /// `convergent_nonce` switches to deterministic content-derived
    /// nonces for intra-tenant dedup; leave it `false` unless the
    /// workspace has opted into convergent encryption (it trades away
    /// forward secrecy for stored ciphertext).
    #[uniffi::constructor]
    pub fn with_object_context(
        dek: Vec<u8>,
        tenant_id: String,
        bucket: String,
        object_key_hash_hex: String,
        version_id: String,
        convergent_nonce: bool,
    ) -> Result<Arc<Self>> {
        let dek = parse_dek(&dek)?;
        let chunk_aad = ChunkAadFormat::Canonical {
            tenant_id,
            bucket,
            object_key_hash_hex,
            version_id,
        }
        .into_bytes();
        Ok(Arc::new(Self {
            dek,
            convergent_nonce,
            chunk_aad,
        }))
    }

    /// Seal `plaintext` into ZK Drive envelope ciphertext.
    ///
    /// The whole buffer is encrypted in one call; callers streaming a
    /// large file should chunk at the application layer and concatenate,
    /// or (preferred for multi-GB files) use the desktop SDK's streaming
    /// path. For the typical mobile case (documents, photos) a single
    /// call is correct and keeps the FFI simple. Blocks the calling
    /// thread; run off the UI thread for large inputs.
    pub fn encrypt(&self, plaintext: Vec<u8>) -> Result<Vec<u8>> {
        encrypt_to_vec(&plaintext, &self.dek, self.options()).map_err(BridgeError::from)
    }

    /// Open envelope `ciphertext` back into plaintext. The engine's DEK
    /// and AAD policy MUST match the ones used to seal the bytes, or the
    /// AEAD tag check fails with [`BridgeError::Crypto`].
    pub fn decrypt(&self, ciphertext: Vec<u8>) -> Result<Vec<u8>> {
        decrypt_to_vec(&ciphertext, &self.dek, self.options()).map_err(BridgeError::from)
    }
}

impl CryptoEngine {
    fn options(&self) -> Options {
        Options {
            chunk_size: None,
            convergent_nonce: self.convergent_nonce,
            chunk_aad: self.chunk_aad.clone(),
        }
    }
}

/// Generate a fresh random 32-byte DEK. Exposed so a native client can
/// mint a per-file key before its first upload without reimplementing a
/// CSPRNG; the key must then be wrapped against the workspace master key
/// (server-side) before the file is shared.
#[uniffi::export]
pub fn generate_dek() -> Vec<u8> {
    DataEncryptionKey::random().as_bytes().to_vec()
}

fn parse_dek(bytes: &[u8]) -> Result<DataEncryptionKey> {
    if bytes.len() != DEK_LEN {
        return Err(BridgeError::InvalidInput(format!(
            "dek must be {DEK_LEN} bytes, got {}",
            bytes.len()
        )));
    }
    DataEncryptionKey::from_bytes(bytes).map_err(BridgeError::from)
}
