import Foundation

/// The composition root: builds and holds every long-lived service once
/// runtime config is known. Created exactly once by `AppBootstrap` and
/// injected into the SwiftUI environment.
@MainActor
final class AppServices {
    let config: AppConfig
    let bridge: BridgeSession
    let keychain: KeychainStore
    let auth: AuthService
    let api: DriveAPIClient
    let offline: OfflineStore
    let transfers: TransferManager
    let sync: SyncCoordinator
    let push: PushManager
    let background: BackgroundSyncScheduler

    init(config: AppConfig) throws {
        self.config = config
        let keychain = KeychainStore()
        self.keychain = keychain
        let bridge = try BridgeSession(config: config)
        self.bridge = bridge
        let oauth = OAuthService(config: config)
        self.auth = AuthService(bridge: bridge, keychain: keychain, oauth: oauth)
        let api = DriveAPIClient(bridge: bridge)
        self.api = api
        let offline = OfflineStore(keychain: keychain)
        self.offline = offline
        self.transfers = TransferManager(bridge: bridge, offlineStore: offline)
        let sync = SyncCoordinator(bridge: bridge)
        self.sync = sync
        self.push = PushManager(api: api)
        self.background = BackgroundSyncScheduler(coordinator: sync)

        // Let the UIKit AppDelegate forward APNs tokens and background
        // URLSession events to these services.
        AppDelegateRouter.shared.push = push
        AppDelegateRouter.shared.transfers = transfers
    }
}

/// Drives app start-up: load config (bundle + server overlay), build
/// services, restore any persisted session. Exposes a `phase` the root
/// view switches on.
@MainActor
final class AppBootstrap: ObservableObject {
    enum Phase {
        case loading
        case ready(AppServices)
        case failed(AppError)
    }

    @Published private(set) var phase: Phase = .loading

    func start() async {
        let config = await AppConfigLoader().load()
        do {
            let services = try AppServices(config: config)
            services.background.register()
            await services.auth.bootstrap()
            phase = .ready(services)
            services.background.scheduleNext()
        } catch {
            phase = .failed(error.asAppError())
        }
    }
}
