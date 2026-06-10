import Foundation
import AuthenticationServices
import UIKit

/// Drives the OIDC Authorization Code + PKCE flow against iam-core using
/// `ASWebAuthenticationSession` (the system browser, so the user's IdP
/// session/cookies and password manager are available and the app never
/// sees credentials).
///
///   1. Build /oauth2/authorize?… with code_challenge + state.
///   2. Present ASWebAuthenticationSession; the IdP redirects to the
///      app's callback URL carrying ?code=&state=.
///   3. Validate state, POST the code + verifier to /oauth2/token.
///   4. Return a bridge `TokenBundle`.
@MainActor
final class OAuthService: NSObject {
    private let config: AppConfig
    private let session: URLSession
    private var webSession: ASWebAuthenticationSession?
    private weak var presentationAnchor: ASPresentationAnchor?

    init(config: AppConfig, session: URLSession = .shared) {
        self.config = config
        self.session = session
    }

    /// Set the window the auth sheet anchors to. Provided by the SwiftUI
    /// layer via a UIWindow lookup.
    func setPresentationAnchor(_ anchor: ASPresentationAnchor?) {
        presentationAnchor = anchor
    }

    /// Run the full interactive flow and return tokens.
    func signIn() async throws -> TokenBundle {
        let pkce = PKCEChallenge()
        let callbackURL = try await authorize(pkce: pkce)
        let code = try extractCode(from: callbackURL, expectedState: pkce.state)
        return try await exchange(code: code, verifier: pkce.verifier)
    }

    // MARK: Steps

    private func authorize(pkce: PKCEChallenge) async throws -> URL {
        var components = URLComponents(url: config.authorizationEndpoint, resolvingAgainstBaseURL: false)
        components?.queryItems = [
            URLQueryItem(name: "response_type", value: "code"),
            URLQueryItem(name: "client_id", value: config.oidcClientID),
            URLQueryItem(name: "redirect_uri", value: config.oidcRedirectURI.absoluteString),
            URLQueryItem(name: "scope", value: config.oidcScopes.joined(separator: " ")),
            URLQueryItem(name: "state", value: pkce.state),
            URLQueryItem(name: "code_challenge", value: pkce.challenge),
            URLQueryItem(name: "code_challenge_method", value: pkce.method),
        ]
        guard let authURL = components?.url else {
            throw AppError(category: .invalidInput, message: "Could not build authorization URL", httpStatus: nil)
        }
        let scheme = config.oidcRedirectURI.scheme

        return try await withCheckedThrowingContinuation { continuation in
            let webSession = ASWebAuthenticationSession(url: authURL, callbackURLScheme: scheme) { callback, error in
                if let error {
                    if let asError = error as? ASWebAuthenticationSessionError, asError.code == .canceledLogin {
                        continuation.resume(throwing: AppError(category: .cancelled, message: "Sign-in was cancelled", httpStatus: nil))
                    } else {
                        continuation.resume(throwing: AppError(category: .auth, message: error.localizedDescription, httpStatus: nil))
                    }
                    return
                }
                guard let callback else {
                    continuation.resume(throwing: AppError(category: .auth, message: "No callback URL returned", httpStatus: nil))
                    return
                }
                continuation.resume(returning: callback)
            }
            webSession.presentationContextProvider = self
            // Use an ephemeral session so a shared-device sign-out is
            // clean: no IdP cookie lingers between users, preventing one
            // user from silently inheriting another's IdP session.
            webSession.prefersEphemeralWebBrowserSession = true
            self.webSession = webSession
            if !webSession.start() {
                continuation.resume(throwing: AppError(category: .auth, message: "Could not start the sign-in session", httpStatus: nil))
            }
        }
    }

    private func extractCode(from url: URL, expectedState: String) throws -> String {
        guard let components = URLComponents(url: url, resolvingAgainstBaseURL: false) else {
            throw AppError(category: .auth, message: "Malformed callback URL", httpStatus: nil)
        }
        let items = components.queryItems ?? []
        if let error = items.first(where: { $0.name == "error" })?.value {
            let description = items.first(where: { $0.name == "error_description" })?.value ?? error
            throw AppError(category: .auth, message: description, httpStatus: nil)
        }
        guard let state = items.first(where: { $0.name == "state" })?.value, state == expectedState else {
            // A mismatched state means a possible CSRF/replay — refuse.
            throw AppError(category: .auth, message: "State mismatch; sign-in aborted for safety", httpStatus: nil)
        }
        guard let code = items.first(where: { $0.name == "code" })?.value, !code.isEmpty else {
            throw AppError(category: .auth, message: "No authorization code in callback", httpStatus: nil)
        }
        return code
    }

    private func exchange(code: String, verifier: String) async throws -> TokenBundle {
        var request = URLRequest(url: config.tokenEndpoint)
        request.httpMethod = "POST"
        request.setValue("application/x-www-form-urlencoded", forHTTPHeaderField: "Content-Type")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        let form: [String: String] = [
            "grant_type": "authorization_code",
            "code": code,
            "redirect_uri": config.oidcRedirectURI.absoluteString,
            "client_id": config.oidcClientID,
            "code_verifier": verifier,
        ]
        request.httpBody = Self.formEncode(form).data(using: .utf8)

        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw AppError.network("No HTTP response from token endpoint")
        }
        guard (200..<300).contains(http.statusCode) else {
            let body = String(data: data, encoding: .utf8) ?? ""
            throw AppError.fromHTTP(status: http.statusCode, message: "Token exchange failed: \(body)")
        }
        return try Self.decodeTokenResponse(data)
    }

    // MARK: Helpers

    struct TokenResponse: Decodable {
        let access_token: String
        let refresh_token: String?
        let expires_in: Int64?
        let scope: String?
    }

    static func decodeTokenResponse(_ data: Data) throws -> TokenBundle {
        let decoded = try JSONDecoder().decode(TokenResponse.self, from: data)
        let lifetime = decoded.expires_in ?? 3600
        let expiresAt = Int64(Date().timeIntervalSince1970) + lifetime
        return TokenBundle(
            accessToken: decoded.access_token,
            refreshToken: decoded.refresh_token ?? "",
            expiresAtUnix: expiresAt,
            scope: decoded.scope ?? ""
        )
    }

    static func formEncode(_ params: [String: String]) -> String {
        var allowed = CharacterSet.alphanumerics
        allowed.insert(charactersIn: "-._~")
        return params.map { key, value in
            let k = key.addingPercentEncoding(withAllowedCharacters: allowed) ?? key
            let v = value.addingPercentEncoding(withAllowedCharacters: allowed) ?? value
            return "\(k)=\(v)"
        }.joined(separator: "&")
    }
}

extension OAuthService: ASWebAuthenticationPresentationContextProviding {
    func presentationAnchor(for session: ASWebAuthenticationSession) -> ASPresentationAnchor {
        if let anchor = presentationAnchor { return anchor }
        // Fall back to the app's key window when no explicit anchor is set.
        let scenes = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
        let window = scenes.flatMap { $0.windows }.first { $0.isKeyWindow }
        return window ?? ASPresentationAnchor()
    }
}
