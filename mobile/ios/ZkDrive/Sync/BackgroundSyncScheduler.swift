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

        // `setTaskCompleted` must be called exactly once; calling it twice is
        // undefined behaviour and crashes. The work Task and the OS
        // `expirationHandler` race to finish, and `work.cancel()` only *requests*
        // cancellation (a blocking FFI poll may still be mid-flight), so without
        // this guard both paths could complete the task. Whichever claims the
        // guard first wins; the loser is a no-op.
        let completion = CompletionGuard()

        let work = Task {
            let applied = await coordinator.syncNow()
            if completion.claim() {
                logger.info("Background sync applied \(applied, privacy: .public) changes")
                task.setTaskCompleted(success: !Task.isCancelled)
            }
        }

        // Honour the OS expiration deadline.
        task.expirationHandler = {
            work.cancel()
            if completion.claim() {
                task.setTaskCompleted(success: false)
            }
        }
    }
}

/// One-shot, thread-safe completion latch. `claim()` returns `true` exactly
/// once (for the first caller) so a `BGTask` can be completed from whichever
/// of the work Task or the expiration handler finishes first, never both.
private final class CompletionGuard: @unchecked Sendable {
    private let lock = NSLock()
    private var claimed = false

    func claim() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if claimed { return false }
        claimed = true
        return true
    }
}
