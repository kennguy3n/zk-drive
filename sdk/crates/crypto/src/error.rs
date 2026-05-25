//! Error type for the crypto crate. Mirrors the Go SDK's error
//! categorisation so frontends / logs can render the same human
//! messages across language boundaries.

use thiserror::Error;

/// Result alias used throughout the crate.
pub type Result<T> = std::result::Result<T, Error>;

/// All errors the crypto crate can produce. The categories match
/// the Go SDK's `client_sdk` error sites so a consumer can map them
/// 1:1 across language boundaries.
#[derive(Debug, Error)]
pub enum Error {
    /// The provided DEK was not exactly 32 bytes (XChaCha20-Poly1305
    /// requires a 256-bit key).
    #[error("crypto: dek must be 32 bytes, got {0}")]
    InvalidDekLength(usize),

    /// AEAD seal or open failed. The Poly1305 tag check failed, which
    /// usually means a wrong DEK / wrong AAD / tampered ciphertext.
    #[error("crypto: aead {op}: {message}")]
    Aead { op: &'static str, message: String },

    /// A frame header was shorter than the 28-byte minimum or claimed
    /// a ciphertext length outside `[1, chunk_size + tag_overhead]`.
    #[error("crypto: invalid frame: {0}")]
    InvalidFrame(String),

    /// Reading the underlying source returned an I/O error.
    #[error("crypto: io: {0}")]
    Io(#[from] std::io::Error),

    /// HKDF expand failed. Only reachable when the requested length
    /// is larger than 255 * HashLen, which is unreachable in this
    /// crate; the variant exists so callers do not have to write
    /// `unwrap`.
    #[error("crypto: hkdf expand: {0}")]
    Hkdf(String),

    /// The envelope failed structural validation (missing key id,
    /// missing CMK URI for a non-public mode, unknown mode string,
    /// etc).
    #[error("crypto: envelope validation: {0}")]
    EnvelopeInvalid(String),
}
