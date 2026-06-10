import Foundation
import UserNotifications

@MainActor
final class SettingsViewModel: ObservableObject {
    @Published private(set) var workspace: Workspace?
    @Published private(set) var offlineBytes: Int64 = 0
    @Published private(set) var pushStatus: UNAuthorizationStatus = .notDetermined
    @Published var error: AppError?
    @Published var biometricAvailable = BiometricAuth.isAvailable

    private let api: DriveAPIClient
    private let offline: OfflineStore
    private let push: PushManager

    init(api: DriveAPIClient, offline: OfflineStore, push: PushManager) {
        self.api = api
        self.offline = offline
        self.push = push
    }

    func load() async {
        await refreshOfflineSize()
        await push.refreshAuthorizationStatus()
        pushStatus = push.authorizationStatus
        do {
            workspace = try await api.listWorkspaces().first
        } catch {
            self.error = error.asAppError()
        }
    }

    func refreshOfflineSize() async {
        offlineBytes = offline.totalBytes()
    }

    func clearOfflineCache() async {
        try? offline.evictAll()
        await refreshOfflineSize()
    }

    var storageText: String {
        guard let workspace else { return "—" }
        return "\(Format.bytes(workspace.storageUsedBytes)) of \(Format.bytes(workspace.storageQuotaBytes))"
    }
}
