package com.zkdrive.app.data.drive

import android.app.Notification
import android.content.Context
import android.content.pm.ServiceInfo
import androidx.core.app.NotificationCompat
import androidx.hilt.work.HiltWorker
import androidx.work.CoroutineWorker
import androidx.work.ForegroundInfo
import androidx.work.WorkerParameters
import com.zkdrive.app.R
import com.zkdrive.app.bridge.BridgeHolder
import com.zkdrive.app.domain.EncryptionMode
import com.zkdrive.app.push.NotificationChannels
import dagger.assisted.Assisted
import dagger.assisted.AssistedInject
import java.io.File

/**
 * Uploads one file in the background so transfers survive app backgrounding or
 * death. The picked bytes are staged in cache by the enqueuer; this worker
 * encrypts (for zero-knowledge folders) and PUTs them via [TransferManager],
 * surfacing progress as a foreground notification on the transfers channel.
 */
@HiltWorker
class UploadWorker @AssistedInject constructor(
    @Assisted appContext: Context,
    @Assisted params: WorkerParameters,
    private val transferManager: TransferManager,
    private val bridgeHolder: BridgeHolder,
) : CoroutineWorker(appContext, params) {

    override suspend fun doWork(): Result {
        if (bridgeHolder.current() == null) return Result.retry()

        val cachePath = inputData.getString(KEY_CACHE_PATH) ?: return Result.failure()
        val displayName = inputData.getString(KEY_NAME) ?: return Result.failure()
        val mimeType = inputData.getString(KEY_MIME) ?: "application/octet-stream"
        val folderId = inputData.getString(KEY_FOLDER_ID) ?: return Result.failure()
        val mode = EncryptionMode.fromWire(inputData.getString(KEY_MODE).orEmpty())

        val staged = File(cachePath)
        if (!staged.exists()) return Result.failure()

        setForeground(foregroundInfo(displayName))
        return try {
            transferManager.upload(
                folderId = folderId,
                encryptionMode = mode,
                input = UploadInput(displayName, mimeType, staged.readBytes()),
            )
            Result.success()
        } catch (e: Exception) {
            if (runAttemptCount < MAX_ATTEMPTS) Result.retry() else Result.failure()
        } finally {
            staged.delete()
        }
    }

    private fun foregroundInfo(displayName: String): ForegroundInfo {
        val notification: Notification =
            NotificationCompat.Builder(applicationContext, NotificationChannels.TRANSFERS)
                .setSmallIcon(R.drawable.ic_zk_logo)
                .setContentTitle(applicationContext.getString(R.string.notif_channel_transfers_name))
                .setContentText(displayName)
                .setOngoing(true)
                .setProgress(0, 0, true)
                .build()
        return ForegroundInfo(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
    }

    companion object {
        const val KEY_CACHE_PATH = "cache_path"
        const val KEY_NAME = "display_name"
        const val KEY_MIME = "mime_type"
        const val KEY_FOLDER_ID = "folder_id"
        const val KEY_MODE = "encryption_mode"

        private const val NOTIFICATION_ID = 0x2001
        private const val MAX_ATTEMPTS = 3
    }
}
