//! Per-chunk AEAD AAD construction.
//!
//! The format below is byte-identical to the one in
//! `zk-object-fabric/encryption/client_sdk/sdk.go` (`chunkAADBytes`).
//! Any change here is also a wire-format change and must be made in
//! lockstep with the Go SDK.

/// Returns the AEAD AAD for `chunk_index` given a per-object AAD
/// prefix. When `chunk_aad` is empty the function returns `None` —
/// callers pass `None` to the AEAD which is then equivalent to
/// AAD = `b""` (the Go SDK uses a nil byte slice for the same
/// semantics).
pub fn build(chunk_aad: &[u8], chunk_index: u64) -> Option<Vec<u8>> {
    if chunk_aad.is_empty() {
        return None;
    }
    let mut out = Vec::with_capacity(chunk_aad.len() + 1 + 8);
    out.extend_from_slice(chunk_aad);
    out.push(b'|');
    out.extend_from_slice(&chunk_index.to_be_bytes());
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_prefix_yields_none() {
        assert!(build(b"", 0).is_none());
        assert!(build(b"", 42).is_none());
    }

    #[test]
    fn nonempty_prefix_is_canonical_pipe_join() {
        // Format: chunk_aad || "|" || big-endian uint64(chunk_index)
        let got = build(b"acme|bucket|deadbeef|v1", 7).unwrap();
        let mut expected = b"acme|bucket|deadbeef|v1|".to_vec();
        expected.extend_from_slice(&7u64.to_be_bytes());
        assert_eq!(got, expected);
    }

    #[test]
    fn chunk_index_uses_big_endian_uint64() {
        // 0x0102030405060708 → 01 02 03 04 05 06 07 08
        let got = build(b"x", 0x0102030405060708).unwrap();
        assert_eq!(&got[got.len() - 8..], &[1, 2, 3, 4, 5, 6, 7, 8]);
    }
}
