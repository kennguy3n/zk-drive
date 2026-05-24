//! PKCE (RFC 7636) helpers.

use rand::RngCore;
use sha2::{Digest, Sha256};

/// A PKCE code verifier + S256-derived challenge. Generate fresh per
/// authorization flow; the verifier MUST be sent on the subsequent
/// `/oauth/token` exchange so the server can recompute the challenge.
#[derive(Debug, Clone)]
pub struct PkceChallenge {
    pub verifier: String,
    pub challenge: String,
}

impl PkceChallenge {
    /// Generates a fresh challenge. The verifier is 64 unreserved
    /// base64url-safe characters (well above RFC 7636's 43-byte
    /// minimum); the challenge is SHA-256(verifier) base64url-no-pad.
    pub fn generate() -> Self {
        let verifier = random_verifier();
        let challenge = s256(&verifier);
        Self {
            verifier,
            challenge,
        }
    }
}

fn random_verifier() -> String {
    let mut raw = [0u8; 48];
    rand::rngs::OsRng.fill_bytes(&mut raw);
    base64url(&raw)
}

fn s256(verifier: &str) -> String {
    let digest = Sha256::digest(verifier.as_bytes());
    base64url(&digest)
}

fn base64url(input: &[u8]) -> String {
    const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let (b0, b1, b2) = (
            *chunk.first().unwrap_or(&0),
            *chunk.get(1).unwrap_or(&0),
            *chunk.get(2).unwrap_or(&0),
        );
        let i0 = (b0 >> 2) as usize;
        let i1 = (((b0 & 0b11) << 4) | (b1 >> 4)) as usize;
        let i2 = (((b1 & 0b1111) << 2) | (b2 >> 6)) as usize;
        let i3 = (b2 & 0b111111) as usize;
        out.push(TABLE[i0] as char);
        out.push(TABLE[i1] as char);
        if chunk.len() >= 2 {
            out.push(TABLE[i2] as char);
        }
        if chunk.len() == 3 {
            out.push(TABLE[i3] as char);
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64url_no_padding_matches_known_vector() {
        // RFC 4648 §10: base64url("foobar") == "Zm9vYmFy"
        assert_eq!(base64url(b"foobar"), "Zm9vYmFy");
        // No padding even for incomplete final group.
        assert_eq!(base64url(b"foo"), "Zm9v");
        assert_eq!(base64url(b"fo"), "Zm8");
        assert_eq!(base64url(b"f"), "Zg");
    }

    #[test]
    fn s256_matches_rfc7636_appendix_b() {
        // RFC 7636 Appendix B sample: verifier =
        //   "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
        // challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
        let verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
        let challenge = s256(verifier);
        assert_eq!(challenge, "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM");
    }

    #[test]
    fn generate_yields_unique_pairs() {
        let a = PkceChallenge::generate();
        let b = PkceChallenge::generate();
        assert_ne!(a.verifier, b.verifier);
        assert_ne!(a.challenge, b.challenge);
        // Re-derive to confirm internal consistency.
        assert_eq!(s256(&a.verifier), a.challenge);
        assert_eq!(s256(&b.verifier), b.challenge);
    }
}
