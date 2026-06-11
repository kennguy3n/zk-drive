import Foundation
import UserNotifications
import UIKit
import os

/// Owns APNs registration and device-token lifecycle. On receiving a
/// device token it registers with the backend
/// (`POST /api/push/register-device`). When the server has push disabled
/// it answers 501; we treat that as "push unavailable" rather than an
/// error so the app degrades gracefully.
@MainActor
final class PushManager: NSObject, ObservableObject {
    @Published private(set) var authorizationStatus: UNAuthorizationStatus = .notDetermined
    @Published private(set) var isRegistered = false

    private let api: DriveAPIClient
    private let logger = Logger(subsystem: "com.zkdrive.app", category: "push")
    private var currentToken: String?

    init(api: DriveAPIClient) {
        self.api = api
        super.init()
    }

    /// Read the current authorization status into `authorizationStatus`.
    func refreshAuthorizationStatus() async {
        let settings = await UNUserNotificationCenter.current().notificationSettings()
        authorizationStatus = settings.authorizationStatus
    }

    /// Request alert/badge/sound permission and, if granted, kick off
    /// APNs registration (which calls back into `didRegister(token:)`).
    func requestAuthorization() async {
        do {
            let granted = try await UNUserNotificationCenter.current()
                .requestAuthorization(options: [.alert, .badge, .sound])
            await refreshAuthorizationStatus()
            if granted {
                UIApplication.shared.registerForRemoteNotifications()
            }
        } catch {
            logger.error("Notification authorization failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// Called from the AppDelegate with the raw APNs token.
    func didRegister(deviceToken: Data) {
        let token = deviceToken.map { String(format: "%02x", $0) }.joined()
        currentToken = token
        Task { await self.register(token: token) }
    }

    func didFailToRegister(error: Error) {
        logger.error("APNs registration failed: \(error.localizedDescription, privacy: .public)")
    }

    private func register(token: String) async {
        do {
            try await api.registerDevice(token: token)
            isRegistered = true
        } catch let appError as AppError {
            // 501 Not Implemented → push backend not configured; not fatal.
            if appError.httpStatus == 501 {
                logger.info("Push backend not configured; skipping registration")
            } else {
                logger.error("Device registration failed: \(appError.userMessage, privacy: .public)")
            }
        } catch {
            logger.error("Device registration failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// Unregister the current device token on sign-out.
    func unregisterCurrentToken() async {
        guard let token = currentToken else { return }
        try? await api.unregisterDevice(token: token)
        isRegistered = false
        currentToken = nil
    }
}
