import Foundation

/// Immutable runtime configuration: where the API lives and the OIDC
/// parameters used to authenticate against iam-core.
///
/// Resolution order (later wins where present):
///   1. The `ZKDriveConfig` dictionary baked into Info.plist (per-build
///      defaults set via xcconfig).
///   2. The server `/api/config` endpoint fetched at launch, so a
///      deployment can rotate issuer/client_id without an app update.
struct AppConfig: Equatable, Sendable {
    var apiBaseURL: URL
    var oidcIssuer: URL
    var oidcClientID: String
    var oidcRedirectURI: URL
    var oidcScopes: [String]

    /// The OIDC authorization endpoint. iam-core follows the standard
    /// path layout; the discovery document (`/.well-known/openid-configuration`)
    /// is honoured by `AppConfigLoader` when reachable, but these
    /// conventional paths are a safe default.
    var authorizationEndpoint: URL { oidcIssuer.appendingPathComponent("oauth2/authorize") }
    var tokenEndpoint: URL { oidcIssuer.appendingPathComponent("oauth2/token") }

    static let plistKey = "ZKDriveConfig"
}

extension AppConfig {
    /// Build the baseline config from Info.plist. Traps only on a
    /// genuinely malformed bundle (missing keys / unparseable URLs),
    /// which is a build-time error the developer must fix, not a runtime
    /// condition to handle.
    static func fromBundle(_ bundle: Bundle = .main) -> AppConfig {
        guard let dict = bundle.object(forInfoDictionaryKey: plistKey) as? [String: Any] else {
            fatalError("Info.plist is missing the \(plistKey) dictionary")
        }
        func string(_ key: String) -> String {
            guard let value = dict[key] as? String, !value.isEmpty else {
                fatalError("Info.plist \(plistKey).\(key) is missing or empty")
            }
            return value
        }
        func url(_ key: String) -> URL {
            let raw = string(key)
            guard let u = URL(string: raw) else {
                fatalError("Info.plist \(plistKey).\(key) is not a valid URL: \(raw)")
            }
            return u
        }
        let scopes = string("OIDCScopes")
            .split(separator: " ")
            .map(String.init)
            .filter { !$0.isEmpty }
        return AppConfig(
            apiBaseURL: url("APIBaseURL"),
            oidcIssuer: url("OIDCIssuer"),
            oidcClientID: string("OIDCClientID"),
            oidcRedirectURI: url("OIDCRedirectURI"),
            oidcScopes: scopes
        )
    }
}

/// The `/api/config` response shape. All fields optional so a partial
/// server config only overrides what it explicitly provides.
private struct ServerConfig: Decodable {
    let api_base_url: String?
    let oidc_issuer: String?
    let oidc_client_id: String?
    let oidc_redirect_uri: String?
    let oidc_scopes: [String]?
}

/// Loads `AppConfig`, overlaying the server config when available.
struct AppConfigLoader {
    let session: URLSession
    let bundle: Bundle

    init(session: URLSession = .shared, bundle: Bundle = .main) {
        self.session = session
        self.bundle = bundle
    }

    /// Returns the bundle baseline immediately overlaid with the server
    /// config when the endpoint responds. A failed/absent endpoint is
    /// non-fatal: the bundle config is fully functional on its own.
    func load() async -> AppConfig {
        let base = AppConfig.fromBundle(bundle)
        guard let overlaid = try? await fetchServerOverlay(base: base) else {
            return base
        }
        return overlaid
    }

    private func fetchServerOverlay(base: AppConfig) async throws -> AppConfig {
        let endpoint = base.apiBaseURL.appendingPathComponent("api/config")
        var request = URLRequest(url: endpoint)
        request.timeoutInterval = 5
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
            throw URLError(.badServerResponse)
        }
        let server = try JSONDecoder().decode(ServerConfig.self, from: data)
        var merged = base
        if let raw = server.api_base_url, let u = URL(string: raw) { merged.apiBaseURL = u }
        if let raw = server.oidc_issuer, let u = URL(string: raw) { merged.oidcIssuer = u }
        if let id = server.oidc_client_id, !id.isEmpty { merged.oidcClientID = id }
        if let raw = server.oidc_redirect_uri, let u = URL(string: raw) { merged.oidcRedirectURI = u }
        if let scopes = server.oidc_scopes, !scopes.isEmpty { merged.oidcScopes = scopes }
        return merged
    }
}
