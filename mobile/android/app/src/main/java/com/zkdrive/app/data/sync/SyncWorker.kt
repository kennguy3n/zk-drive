package com.zkdrive.app.data.sync

import android.content.Context
import androidx.hilt.work.HiltWorker
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import com.zkdrive.app.bridge.BridgeHolder
import dagger.assisted.Assisted
import dagger.assisted.AssistedInject
import kotlin.coroutines.cancellation.CancellationException

/**
 * Periodic background sync. Pulls the changefeed and reflects it into the
 * offline catalogue via [SyncCoordinator]. No-op (success) when signed out so
 * WorkManager doesn't retry pointlessly.
 */
@HiltWorker
class SyncWorker @AssistedInject constructor(
    @Assisted appContext: Context,
    @Assisted params: WorkerParameters,
    private val coordinator: SyncCoordinator,
    private val bridgeHolder: BridgeHolder,
) : CoroutineWorker(appContext, params) {

    override suspend fun doWork(): Result {
        if (bridgeHolder.current() == null) return Result.success()
        return try {
            coordinator.syncNow()
            Result.success()
        } catch (e: CancellationException) {
            // WorkManager stopped us (constraints lost / cancelled): honour the
            // coroutine cancellation contract and propagate instead of retrying.
            throw e
        } catch (e: Exception) {
            // Transient (network/storage) — let WorkManager back off and retry.
            Result.retry()
        }
    }
}
