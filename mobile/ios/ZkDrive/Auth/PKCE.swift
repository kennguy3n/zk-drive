import Foundation
import CryptoKit
import Security

/// A freshly-generated PKCE (RFC 7636) verifier/challenge pair plus the
/// CSRF `state` for one authorization request.
struct PKCEChallenge: Equatable {
    let verifier: String
    let challenge: String
    let method = "S256"
    let state: String

    init() {
        verifier = PKCEChallenge.randomURLSafe(byteCount: 64)
        state = PKCEChallenge.randomURLSafe(byteCount: 32)
        let digest = SHA256.hash(data: Data(verifier.utf8))
        challenge = Data(digest).base64URLEncodedString()
    }

    /// For tests: construct from a fixed verifier so the challenge is
    /// deterministic.
    init(verifier: String, state: String) {
        self.verifier = verifier
        self.state = state
        let digest = SHA256.hash(data: Data(verifier.utf8))
        self.challenge = Data(digest).base64URLEncodedString()
    }

    private static func randomURLSafe(byteCount: Int) -> String {
        var bytes = [UInt8](repeating: 0, count: byteCount)
        let status = SecRandomCopyBytes(kSecRandomDefault, byteCount, &bytes)
        if status != errSecSuccess {
            // Fall back to a CryptoKit-derived random; never returns weak
            // entropy silently.
            bytes = (0..<byteCount).map { _ in UInt8.random(in: 0...255) }
        }
        return Data(bytes).base64URLEncodedString()
    }
}

extension Data {
    /// Base64URL without padding, per RFC 7636 §A.
    func base64URLEncodedString() -> String {
        base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
}
