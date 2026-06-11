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
    /// The live background-sync scheduler. The BGTask launch handler is
    /// registered at `didFinishLaunchingWithOptions` (per Apple's contract,
    /// before launch finishes) but the scheduler — like the rest of
    /// `AppServices` — is built asynchronously afterwards, so the handler
    /// resolves it from here when iOS actually runs the task.
    weak var background: BackgroundSyncScheduler?
    /// Held on the router itself (not on `TransferManager`) because iOS can
    /// relaunch the app to deliver background-URLSession events — and this
    /// handler — before `AppServices` has constructed the `TransferManager`.
    /// Storing it here means it survives that window; `TransferManager`'s
    /// `urlSessionDidFinishEvents` calls and clears it once all events for the
    /// reconnected background session have been delivered.
    var pendingBackgroundCompletionHandler: (() -> Void)?
    private init() {}
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        UNUserNotificationCenter.current().delegate = self
        // `BGTaskScheduler.register` must run before launch finishes, so it
        // happens here rather than from the async `AppServices` bootstrap. The
        // handler resolves the live scheduler from `AppDelegateRouter` at fire
        // time (it's wired in once the services graph is built).
        BackgroundSyncScheduler.registerLaunchHandler()
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
    // Store the completion handler unconditionally on the router so it isn't
    // lost when this fires during an early relaunch (before TransferManager
    // exists). TransferManager calls it once it has finished processing the
    // delivered background events.
    func application(_ application: UIApplication,
                     handleEventsForBackgroundURLSession identifier: String,
                     completionHandler: @escaping () -> Void) {
        Task { @MainActor in
            AppDelegateRouter.shared.pendingBackgroundCompletionHandler = completionHandler
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
