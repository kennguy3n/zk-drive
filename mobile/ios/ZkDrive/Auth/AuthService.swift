import Foundation
import Combine

/// The app's authentication state machine, observable by the UI.
@MainActor
final class AuthService: ObservableObject {
    enum State: Equatable {
        case loading              // checking persisted credentials
        case signedOut            // no credentials → show Login
        case locked               // credentials present, biometric unlock required
        case signedIn             // tokens loaded into the bridge
    }

    @Published private(set) var state: State = .loading
    @Published private(set) var identity: IdentityClaims?
    @Published var lastError: AppError?

    private let bridge: BridgeSession
    private let keychain: KeychainStore
    private let oauth: OAuthService
    private let defaults: UserDefaults

    private static let tokenAccount = "oidc-token-bundle"
    static let biometricLockKey = "settings.biometricLockEnabled"

    init(bridge: BridgeSession, keychain: KeychainStore, oauth: OAuthService, defaults: UserDefaults = .standard) {
        self.bridge = bridge
        self.keychain = keychain
        self.oauth = oauth
        self.defaults = defaults
    }

    var biometricLockEnabled: Bool {
        get { defaults.bool(forKey: Self.biometricLockKey) }
        set { defaults.set(newValue, forKey: Self.biometricLockKey) }
    }

    /// Restore a persisted session on launch.
    func bootstrap() async {
        do {
            guard let stored = try keychain.read(StoredTokenBundle.self, account: Self.tokenAccount) else {
                state = .signedOut
                return
            }
            if biometricLockEnabled && BiometricAuth.isAvailable {
                // Tokens stay out of the bridge until biometric unlock.
                state = .locked
            } else {
                try await loadIntoBridge(stored)
                state = .signedIn
            }
        } catch {
            lastError = error.asAppError()
            state = .signedOut
        }
    }

    /// Interactive sign-in via the system browser.
    func signIn() async {
        do {
            let bundle = try await oauth.signIn()
            try await bridge.setTokens(bundle)
            try persist(bundle)
            identity = IdentityClaims.decode(jwt: bundle.accessToken)
            lastError = nil
            state = .signedIn
        } catch {
            let appError = error.asAppError()
            // A user-cancelled sign-in is not an error worth surfacing.
            if appError.category != .cancelled {
                lastError = appError
            }
        }
    }

    /// Unlock a `locked` session with Face ID / Touch ID.
    func unlockWithBiometrics() async {
        do {
            try await BiometricAuth.authenticate(reason: "Unlock your ZK Drive")
            guard let stored = try keychain.read(StoredTokenBundle.self, account: Self.tokenAccount) else {
                state = .signedOut
                return
            }
            try await loadIntoBridge(stored)
            lastError = nil
            state = .signedIn
        } catch {
            lastError = error.asAppError()
        }
    }

    /// Sign out: clear the bridge tokens and wipe persisted credentials.
    func signOut() async {
        try? await bridge.clearTokens()
        try? keychain.delete(account: Self.tokenAccount)
        identity = nil
        state = .signedOut
    }

    // MARK: Helpers

    private func loadIntoBridge(_ stored: StoredTokenBundle) async throws {
        try await bridge.setTokens(stored.bridgeBundle)
        identity = IdentityClaims.decode(jwt: stored.bridgeBundle.accessToken)
    }

    private func persist(_ bundle: TokenBundle) throws {
        try keychain.write(StoredTokenBundle(bundle), account: Self.tokenAccount)
    }
}
