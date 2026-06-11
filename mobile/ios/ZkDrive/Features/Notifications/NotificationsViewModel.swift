import Foundation
import UserNotifications

@MainActor
final class NotificationsViewModel: ObservableObject {
    @Published private(set) var notifications: [AppNotification] = []
    @Published private(set) var isLoading = false
    @Published var error: AppError?
    @Published var pushStatus: UNAuthorizationStatus = .notDetermined

    private let api: DriveAPIClient
    private let push: PushManager

    init(api: DriveAPIClient, push: PushManager) {
        self.api = api
        self.push = push
    }

    var unreadCount: Int { notifications.filter { !$0.isRead }.count }

    func load() async {
        isLoading = true
        defer { isLoading = false }
        await push.refreshAuthorizationStatus()
        pushStatus = push.authorizationStatus
        do {
            notifications = try await api.listNotifications()
            error = nil
        } catch let appError as AppError where appError.httpStatus == 501 {
            // Notifications backend not configured; show empty, not error.
            notifications = []
        } catch {
            self.error = error.asAppError()
        }
    }

    func enablePush() async {
        await push.requestAuthorization()
        await push.refreshAuthorizationStatus()
        pushStatus = push.authorizationStatus
    }

    func markRead(_ notification: AppNotification) async {
        guard !notification.isRead else { return }
        do {
            try await api.markNotificationRead(id: notification.id)
            if let idx = notifications.firstIndex(where: { $0.id == notification.id }) {
                notifications[idx] = AppNotification(
                    id: notification.id, type: notification.type, title: notification.title,
                    body: notification.body, resourceType: notification.resourceType,
                    resourceID: notification.resourceID, readAt: Date(), createdAt: notification.createdAt
                )
            }
        } catch {
            self.error = error.asAppError()
        }
    }

    func markAllRead() async {
        do {
            try await api.markAllNotificationsRead()
            await load()
        } catch {
            self.error = error.asAppError()
        }
    }
}
