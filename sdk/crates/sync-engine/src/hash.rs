//! Content hashing.
//!
//! BLAKE3 was chosen over SHA-256 to match the zk-object-fabric chunk
//! boundaries (which are content-defined via BLAKE3) and because it's
//! ~5× faster on commodity hardware. The 32-byte output is what the
//! catalogue stores.

use std::io::Read;

/// Computes the BLAKE3-256 of an entire reader's content. The reader
/// is consumed in 64 KiB chunks so the SDK never allocates more than
/// a single chunk's worth of plaintext at once.
pub fn content_hash<R: Read>(mut r: R) -> std::io::Result<[u8; 32]> {
    let mut hasher = blake3::Hasher::new();
    let mut buf = [0u8; 64 * 1024];
    loop {
        let n = r.read(&mut buf)?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    let mut out = [0u8; 32];
    out.copy_from_slice(hasher.finalize().as_bytes());
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hash_matches_blake3_reference() {
        // BLAKE3 of empty input
        let h = content_hash(std::io::empty()).unwrap();
        let expected = *blake3::hash(b"").as_bytes();
        assert_eq!(h, expected);
    }

    #[test]
    fn hash_streaming_matches_one_shot() {
        let bytes: Vec<u8> = (0..200_000).map(|i| (i % 251) as u8).collect();
        let h = content_hash(&bytes[..]).unwrap();
        let one_shot = blake3::hash(&bytes);
        assert_eq!(&h, one_shot.as_bytes());
    }
}
