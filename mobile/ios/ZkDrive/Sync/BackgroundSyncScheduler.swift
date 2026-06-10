import Foundation
import BackgroundTasks
import os

/// Registers and schedules the periodic background sync via
/// `BGAppRefreshTask`. iOS decides exactly when to run based on usage
/// patterns; we request "no earlier than 15 minutes" and re-schedule on
/// every execution so the cadence is self-sustaining.
@MainActor
final class BackgroundSyncScheduler {
    static let taskIdentifier = "com.zkdrive.app.sync.refresh"
    private static let minimumInterval: TimeInterval = 15 * 60

    private let coordinator: SyncCoordinator
    private let logger = Logger(subsystem: "com.zkdrive.app", category: "background-sync")

    init(coordinator: SyncCoordinator) {
        self.coordinator = coordinator
    }

    /// Register the launch handler. Must be called before the app
    /// finishes launching (per `BGTaskScheduler` contract).
    func register() {
        // The launch handler is invoked on a non-main queue. `handle(_:)` and
        // everything it touches (`coordinator`, `logger`) are `@MainActor`, so
        // hop onto the main actor before doing any work to avoid a data race.
        BGTaskScheduler.shared.register(forTaskWithIdentifier: Self.taskIdentifier, using: nil) { [weak self] task in
            guard let refreshTask = task as? BGAppRefreshTask else {
                task.setTaskCompleted(success: false)
                return
            }
            Task { @MainActor [weak self] in
                guard let self else {
                    refreshTask.setTaskCompleted(success: false)
                    return
                }
                self.handle(refreshTask)
            }
        }
    }

    /// Ask iOS to schedule the next refresh. Called at launch and after
    /// each run. Quietly ignores submission errors (e.g. simulator).
    func scheduleNext() {
        let request = BGAppRefreshTaskRequest(identifier: Self.taskIdentifier)
        request.earliestBeginDate = Date(timeIntervalSinceNow: Self.minimumInterval)
        do {
            try BGTaskScheduler.shared.submit(request)
            logger.debug("Scheduled next background sync")
        } catch {
            logger.error("Failed to schedule background sync: \(error.localizedDescription, privacy: .public)")
        }
    }

    private func handle(_ task: BGAppRefreshTask) {
        // Always line up the next occurrence first so a crash mid-sync
        // doesn't break the cadence.
        scheduleNext()

        let work = Task {
            let applied = await coordinator.syncNow()
            logger.info("Background sync applied \(applied, privacy: .public) changes")
            task.setTaskCompleted(success: true)
        }

        // Honour the OS expiration deadline.
        task.expirationHandler = {
            work.cancel()
            task.setTaskCompleted(success: false)
        }
    }
}
