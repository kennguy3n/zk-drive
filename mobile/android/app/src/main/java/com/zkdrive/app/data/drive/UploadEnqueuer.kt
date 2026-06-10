package com.zkdrive.app.data.drive

import android.content.Context
import android.net.Uri
import android.provider.OpenableColumns
import androidx.work.Constraints
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.OutOfQuotaPolicy
import androidx.work.WorkManager
import androidx.work.workDataOf
import com.zkdrive.app.di.IoDispatcher
import com.zkdrive.app.domain.EncryptionMode
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.withContext
import java.io.File
import java.util.UUID
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Stages a picked (SAF / camera) file into cache and enqueues an
 * [UploadWorker] so the transfer completes even if the user leaves the app.
 * Staging is required because the originating content Uri permission may not
 * outlive the foreground task.
 */
@Singleton
class UploadEnqueuer @Inject constructor(
    @ApplicationContext private val context: Context,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    suspend fun enqueue(uri: Uri, folderId: String, mode: EncryptionMode): String =
        withContext(io) {
            val displayName = resolveDisplayName(uri)
            val mimeType = context.contentResolver.getType(uri) ?: "application/octet-stream"
            val staged = stage(uri)

            val request = OneTimeWorkRequestBuilder<UploadWorker>()
                .setConstraints(
                    Constraints.Builder()
                        .setRequiredNetworkType(NetworkType.CONNECTED)
                        .build(),
                )
                .setExpedited(OutOfQuotaPolicy.RUN_AS_NON_EXPEDITED_WORK_REQUEST)
                .setInputData(
                    workDataOf(
                        UploadWorker.KEY_CACHE_PATH to staged.absolutePath,
                        UploadWorker.KEY_NAME to displayName,
                        UploadWorker.KEY_MIME to mimeType,
                        UploadWorker.KEY_FOLDER_ID to folderId,
                        UploadWorker.KEY_MODE to mode.wire,
                    ),
                )
                .addTag(TAG)
                .build()

            WorkManager.getInstance(context).enqueue(request)
            displayName
        }

    private fun stage(uri: Uri): File {
        val dir = File(context.cacheDir, "uploads").apply { mkdirs() }
        val dest = File(dir, UUID.randomUUID().toString())
        context.contentResolver.openInputStream(uri)?.use { input ->
            dest.outputStream().use { output -> input.copyTo(output) }
        } ?: throw java.io.IOException("Unable to open $uri")
        return dest
    }

    private fun resolveDisplayName(uri: Uri): String {
        context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
            ?.use { cursor ->
                if (cursor.moveToFirst()) {
                    val idx = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                    if (idx >= 0) {
                        val name = cursor.getString(idx)
                        if (!name.isNullOrBlank()) return name
                    }
                }
            }
        return uri.lastPathSegment?.substringAfterLast('/') ?: "upload"
    }

    private companion object {
        const val TAG = "zk_upload"
    }
}
