//! Encryption envelope types — the JSON-serialisable metadata that
//! accompanies every encrypted object. These structs deserialise from
//! the wire format emitted by `zk-object-fabric/encryption/envelope.go`
//! and re-serialise to a byte-identical form.

use serde::{Deserialize, Serialize};

use crate::error::{Error, Result};

/// EncryptionMode names the operating mode for an object's encryption.
/// The string values are the canonical ones used by both the Go and
/// the Rust SDKs so envelope JSON round-trips cleanly across languages.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum EncryptionMode {
    /// Client SDK encrypts. Plaintext keys never reach the service.
    #[serde(rename = "client_side")]
    StrictZk,
    /// The Linode gateway encrypts. The gateway sees plaintext in
    /// memory during request handling.
    #[serde(rename = "managed")]
    ManagedEncrypted,
    /// Ciphertext at rest, served as plaintext at the edge.
    #[serde(rename = "public_distribution")]
    PublicDistribution,
}

impl EncryptionMode {
    /// Returns true for any of the three defined modes. The enum is
    /// already exhaustive in Rust; the helper exists for parity with
    /// the Go SDK's `Valid()` method so logging code can be ported
    /// verbatim.
    pub const fn valid(self) -> bool {
        matches!(
            self,
            EncryptionMode::StrictZk
                | EncryptionMode::ManagedEncrypted
                | EncryptionMode::PublicDistribution
        )
    }
}

/// A 32-byte XChaCha20-Poly1305 key used to seal individual chunks.
/// The plaintext DEK is held only transiently in memory by the SDK;
/// it is wrapped by the customer master key for at-rest storage.
#[derive(Clone)]
pub struct DataEncryptionKey([u8; 32]);

impl DataEncryptionKey {
    /// Wrap an existing 32-byte buffer. Returns
    /// [`Error::InvalidDekLength`] when the buffer is not exactly
    /// 32 bytes.
    pub fn from_bytes(bytes: &[u8]) -> Result<Self> {
        if bytes.len() != 32 {
            return Err(Error::InvalidDekLength(bytes.len()));
        }
        let mut k = [0u8; 32];
        k.copy_from_slice(bytes);
        Ok(Self(k))
    }

    /// Generate a fresh random DEK using the platform CSPRNG. Same
    /// entropy source the Go SDK uses (`crypto/rand`).
    pub fn random() -> Self {
        use rand::RngCore;
        let mut k = [0u8; 32];
        rand::rngs::OsRng.fill_bytes(&mut k);
        Self(k)
    }

    /// Borrow the raw bytes. The slice has the same lifetime as the
    /// `DataEncryptionKey`; callers must not retain it beyond the
    /// key's drop.
    pub fn as_bytes(&self) -> &[u8; 32] {
        &self.0
    }
}

impl std::fmt::Debug for DataEncryptionKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Never print the DEK bytes — even at debug level. The
        // Go SDK has the equivalent redacted Stringer.
        f.write_str("DataEncryptionKey(<redacted>)")
    }
}

/// The wrapped (ciphertext) form of a [`DataEncryptionKey`] that
/// is safe to persist in the manifest. The unwrap path lives in the
/// `auth` crate's KMS / Vault adapter.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WrappedDataEncryptionKey {
    /// Stable identifier for this DEK; recorded on the manifest.
    pub key_id: String,
    /// Content-encryption algorithm the DEK is used with
    /// (e.g. "xchacha20-poly1305").
    pub algorithm: String,
    /// The opaque, CMK-wrapped DEK bytes.
    pub wrapped_key: Vec<u8>,
    /// Algorithm used to wrap the DEK
    /// (e.g. "aes-256-gcm-wrap", "rsa-oaep-sha256").
    pub wrap_algorithm: String,
}

/// Reference to the customer master key used to wrap DEKs.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CustomerMasterKeyRef {
    /// Opaque key locator, e.g. "cmk://acme/prod/root" or
    /// "aws-kms://arn:aws:kms:...".
    pub uri: String,
    /// Monotonic generation number that advances on rotation.
    pub version: i64,
    /// Who holds the plaintext master key.
    /// Valid values: "customer", "gateway_hsm", "none".
    pub holder_class: String,
}

/// Per-object encryption descriptor attached to every manifest.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EncryptionEnvelope {
    pub mode: EncryptionMode,
    pub dek: WrappedDataEncryptionKey,
    pub cmk: CustomerMasterKeyRef,
    pub manifest_encrypted: bool,
}

impl EncryptionEnvelope {
    /// Minimal structural validation; mirrors `Validate()` in
    /// `zk-object-fabric/encryption/envelope.go`. The fields are
    /// re-checked here so a Rust decoder rejects the same malformed
    /// envelopes a Go decoder would.
    pub fn validate(&self) -> Result<()> {
        if !self.mode.valid() {
            return Err(Error::EnvelopeInvalid(format!(
                "unknown mode {:?}",
                self.mode
            )));
        }
        if self.dek.key_id.is_empty() {
            return Err(Error::EnvelopeInvalid("dek.key_id is required".into()));
        }
        if self.dek.algorithm.is_empty() {
            return Err(Error::EnvelopeInvalid("dek.algorithm is required".into()));
        }
        if matches!(self.mode, EncryptionMode::PublicDistribution) {
            return Ok(());
        }
        if self.cmk.uri.is_empty() {
            return Err(Error::EnvelopeInvalid(format!(
                "cmk.uri is required for mode {:?}",
                self.mode
            )));
        }
        if self.cmk.holder_class.is_empty() {
            return Err(Error::EnvelopeInvalid(format!(
                "cmk.holder_class is required for mode {:?}",
                self.mode
            )));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mode_round_trips_through_canonical_json() {
        for (mode, expected) in [
            (EncryptionMode::StrictZk, "\"client_side\""),
            (EncryptionMode::ManagedEncrypted, "\"managed\""),
            (
                EncryptionMode::PublicDistribution,
                "\"public_distribution\"",
            ),
        ] {
            let got = serde_json::to_string(&mode).unwrap();
            assert_eq!(
                got, expected,
                "{:?} did not serialise to {}",
                mode, expected
            );
            let decoded: EncryptionMode = serde_json::from_str(expected).unwrap();
            assert_eq!(decoded, mode);
        }
    }

    #[test]
    fn validate_rejects_missing_fields() {
        let mut env = EncryptionEnvelope {
            mode: EncryptionMode::StrictZk,
            dek: WrappedDataEncryptionKey {
                key_id: String::new(),
                algorithm: "xchacha20-poly1305".into(),
                wrapped_key: vec![],
                wrap_algorithm: "aes-256-gcm-wrap".into(),
            },
            cmk: CustomerMasterKeyRef {
                uri: "cmk://t/root".into(),
                version: 1,
                holder_class: "customer".into(),
            },
            manifest_encrypted: true,
        };
        assert!(matches!(env.validate(), Err(Error::EnvelopeInvalid(_))));
        env.dek.key_id = "k1".into();
        assert!(env.validate().is_ok());
        // Public distribution may omit cmk.
        env.mode = EncryptionMode::PublicDistribution;
        env.cmk.uri.clear();
        env.cmk.holder_class.clear();
        assert!(env.validate().is_ok());
    }

    #[test]
    fn dek_redacts_in_debug() {
        let d = DataEncryptionKey::random();
        let s = format!("{:?}", d);
        assert!(s.contains("<redacted>"), "got {s}");
    }
}
