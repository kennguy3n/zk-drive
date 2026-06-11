import XCTest
@testable import ZkDrive

/// Covers the pure token-exchange helpers of the OIDC PKCE flow: form
/// encoding of the token request and decoding the IdP token response into
/// the bridge's `TokenBundle`.
@MainActor
final class OAuthServiceTests: XCTestCase {
    func testFormEncodePercentEncodesReservedCharacters() {
        let encoded = OAuthService.formEncode(["redirect_uri": "zkdrive://oauth/callback"])
        // ':' and '/' are reserved and must be percent-encoded; only
        // RFC 3986 unreserved chars (-._~ + alphanumerics) survive raw.
        XCTAssertEqual(encoded, "redirect_uri=zkdrive%3A%2F%2Foauth%2Fcallback")
    }

    func testFormEncodeJoinsPairsWithAmpersand() {
        let encoded = OAuthService.formEncode(["a": "1", "b": "2"])
        let pairs = Set(encoded.split(separator: "&").map(String.init))
        XCTAssertEqual(pairs, ["a=1", "b=2"])
    }

    func testDecodeTokenResponseMapsAllFields() throws {
        let json = """
        {"access_token":"at-1","refresh_token":"rt-1","expires_in":1200,"scope":"openid email"}
        """.data(using: .utf8)!

        let before = Int64(Date().timeIntervalSince1970)
        let bundle = try OAuthService.decodeTokenResponse(json)
        let after = Int64(Date().timeIntervalSince1970)

        XCTAssertEqual(bundle.accessToken, "at-1")
        XCTAssertEqual(bundle.refreshToken, "rt-1")
        XCTAssertEqual(bundle.scope, "openid email")
        XCTAssertGreaterThanOrEqual(bundle.expiresAtUnix, before + 1200)
        XCTAssertLessThanOrEqual(bundle.expiresAtUnix, after + 1200)
    }

    func testDecodeTokenResponseAppliesDefaults() throws {
        // No refresh_token, expires_in, or scope → safe defaults.
        let json = #"{"access_token":"at-only"}"#.data(using: .utf8)!
        let before = Int64(Date().timeIntervalSince1970)
        let bundle = try OAuthService.decodeTokenResponse(json)

        XCTAssertEqual(bundle.accessToken, "at-only")
        XCTAssertEqual(bundle.refreshToken, "")
        XCTAssertEqual(bundle.scope, "")
        // Default lifetime is one hour.
        XCTAssertGreaterThanOrEqual(bundle.expiresAtUnix, before + 3600)
    }

    func testDecodeTokenResponseThrowsOnMissingAccessToken() {
        let json = #"{"refresh_token":"rt"}"#.data(using: .utf8)!
        XCTAssertThrowsError(try OAuthService.decodeTokenResponse(json))
    }
}
