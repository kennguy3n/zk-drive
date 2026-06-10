import UIKit
import UserNotifications

/// Routes UIKit AppDelegate callbacks (APNs tokens, background URLSession
/// events) to services that are constructed asynchronously after launch.
/// The delegate fires before `AppServices` exists, so it forwards through
/// this shared router which the services wire themselves into once ready.
@MainActor
final class AppDelegateRouter {
    static let shared = AppDelegateRouter()
    weak var push: PushManager?
    weak var transfers: TransferManager?
    private init() {}
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        UNUserNotificationCenter.current().delegate = self
        return true
    }

    // MARK: APNs

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        Task { @MainActor in AppDelegateRouter.shared.push?.didRegister(deviceToken: deviceToken) }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        Task { @MainActor in AppDelegateRouter.shared.push?.didFailToRegister(error: error) }
    }

    // MARK: Background URLSession
    //
    // Store the completion handler so `TransferManager` can call it once
    // it has finished processing delivered background events.
    func application(_ application: UIApplication,
                     handleEventsForBackgroundURLSession identifier: String,
                     completionHandler: @escaping () -> Void) {
        Task { @MainActor in
            AppDelegateRouter.shared.transfers?.backgroundCompletionHandler = completionHandler
        }
    }
}

extension AppDelegate: UNUserNotificationCenterDelegate {
    // Show banners/sounds even when the app is in the foreground.
    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .badge, .sound]
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                didReceive response: UNNotificationResponse) async {
        // Tapping a notification posts a name the UI observes to deep-link.
        NotificationCenter.default.post(name: .zkDriveDidTapNotification, object: response.notification.request.content.userInfo)
    }
}

extension Notification.Name {
    static let zkDriveDidTapNotification = Notification.Name("zkDriveDidTapNotification")
}
