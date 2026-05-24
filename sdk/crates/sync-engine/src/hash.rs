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
///
/// `ErrorKind::Interrupted` is retried transparently to match the
/// crypto crate's `read_full` and the standard library's
/// `Read::read_to_end` convention. A signal-interrupted read here
/// would otherwise surface as a hash failure and cause the watcher
/// to mark a file dirty even though it's actually unchanged.
pub fn content_hash<R: Read>(mut r: R) -> std::io::Result<[u8; 32]> {
    let mut hasher = blake3::Hasher::new();
    let mut buf = [0u8; 64 * 1024];
    loop {
        match r.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => hasher.update(&buf[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
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

    /// Reader that returns `Interrupted` once before each real read.
    /// Ensures `content_hash` retries on EINTR instead of bubbling it
    /// up as a hash failure.
    struct EintrOnceThenRead<'a> {
        inner: &'a [u8],
        pos: usize,
        eintr_pending: bool,
    }

    impl<'a> Read for EintrOnceThenRead<'a> {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            if self.eintr_pending {
                self.eintr_pending = false;
                return Err(std::io::Error::from(std::io::ErrorKind::Interrupted));
            }
            self.eintr_pending = true;
            if self.pos >= self.inner.len() {
                return Ok(0);
            }
            let n = (self.inner.len() - self.pos).min(buf.len());
            buf[..n].copy_from_slice(&self.inner[self.pos..self.pos + n]);
            self.pos += n;
            Ok(n)
        }
    }

    #[test]
    fn hash_retries_on_eintr() {
        let payload: Vec<u8> = (0..4096).map(|i| (i % 251) as u8).collect();
        let reader = EintrOnceThenRead {
            inner: &payload,
            pos: 0,
            eintr_pending: true,
        };
        let h = content_hash(reader).unwrap();
        assert_eq!(&h, blake3::hash(&payload).as_bytes());
    }
}
