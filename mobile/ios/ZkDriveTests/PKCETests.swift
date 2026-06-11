import XCTest
@testable import ZkDrive

/// Verifies the PKCE (RFC 7636) verifier/challenge derivation against the
/// canonical test vector from the spec, plus the structural invariants
/// the IdP relies on.
final class PKCETests: XCTestCase {
    /// RFC 7636 Appendix B worked example.
    func testChallengeMatchesRFC7636Vector() {
        let verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
        let expectedChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

        let pkce = PKCEChallenge(verifier: verifier, state: "state-123")

        XCTAssertEqual(pkce.challenge, expectedChallenge)
        XCTAssertEqual(pkce.method, "S256")
        XCTAssertEqual(pkce.verifier, verifier)
        XCTAssertEqual(pkce.state, "state-123")
    }

    func testBase64URLEncodingHasNoPaddingOrUnsafeChars() {
        // Bytes chosen so the standard base64 form contains '+', '/', '='.
        let data = Data([0xff, 0xfe, 0xfd, 0xfc, 0xfb])
        let encoded = data.base64URLEncodedString()

        XCTAssertFalse(encoded.contains("+"))
        XCTAssertFalse(encoded.contains("/"))
        XCTAssertFalse(encoded.contains("="))
    }

    func testGeneratedPairIsUniqueAndWellFormed() {
        let a = PKCEChallenge()
        let b = PKCEChallenge()

        // Random per instance.
        XCTAssertNotEqual(a.verifier, b.verifier)
        XCTAssertNotEqual(a.state, b.state)

        // RFC 7636 requires 43..128 char verifiers; 64 random bytes
        // base64url-encoded lands comfortably inside that range.
        XCTAssertGreaterThanOrEqual(a.verifier.count, 43)
        XCTAssertLessThanOrEqual(a.verifier.count, 128)

        // The challenge must be the deterministic SHA-256 of the verifier.
        let recomputed = PKCEChallenge(verifier: a.verifier, state: a.state)
        XCTAssertEqual(a.challenge, recomputed.challenge)
    }
}
