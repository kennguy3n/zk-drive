import Foundation
import SwiftUI

/// Centralised keys + small helpers for user preferences persisted in
/// `UserDefaults` via `@AppStorage`. Keeping the keys here prevents typos
/// across the many call sites that read them.
enum SettingsStore {
    static let appearanceKey = "settings.appearance"
    static let defaultListLayoutKey = "settings.defaultListLayout"
    static let wifiOnlySyncKey = "settings.wifiOnlySync"
    static let autoOfflineKey = "settings.autoOfflinePinned"
    static let biometricLockKey = AuthService.biometricLockKey
    static let notificationsMutedKey = "settings.notificationsMuted"
}

/// File browser layout, persisted so the user's last choice sticks.
enum BrowserLayout: String, CaseIterable, Identifiable {
    case list
    case grid
    var id: String { rawValue }
    var systemImage: String { self == .list ? "list.bullet" : "square.grid.2x2" }
    var toggled: BrowserLayout { self == .list ? .grid : .list }
}
