package com.zkdrive.app

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager
import androidx.work.Configuration
import com.zkdrive.app.push.NotificationChannels
import dagger.hilt.android.HiltAndroidApp
import javax.inject.Inject

/**
 * Application entry point.
 *
 * Implements [Configuration.Provider] so WorkManager picks up the Hilt worker
 * factory (the default initializer is disabled in the manifest), letting
 * background sync workers receive injected dependencies.
 */
@HiltAndroidApp
class ZkDriveApplication : Application(), Configuration.Provider {

    @Inject
    lateinit var workerFactory: androidx.hilt.work.HiltWorkerFactory

    override val workManagerConfiguration: Configuration
        get() = Configuration.Builder()
            .setWorkerFactory(workerFactory)
            .setMinimumLoggingLevel(if (BuildConfig.DEBUG) android.util.Log.DEBUG else android.util.Log.WARN)
            .build()

    override fun onCreate() {
        super.onCreate()
        registerNotificationChannels()
    }

    private fun registerNotificationChannels() {
        val manager = getSystemService(NotificationManager::class.java)
        val channels = listOf(
            NotificationChannel(
                getString(R.string.notif_channel_push),
                getString(R.string.notif_channel_push_name),
                NotificationManager.IMPORTANCE_DEFAULT,
            ),
            NotificationChannel(
                getString(R.string.notif_channel_transfers),
                getString(R.string.notif_channel_transfers_name),
                NotificationManager.IMPORTANCE_LOW,
            ).apply { setShowBadge(false) },
            NotificationChannel(
                getString(R.string.notif_channel_sync),
                getString(R.string.notif_channel_sync_name),
                NotificationManager.IMPORTANCE_MIN,
            ).apply { setShowBadge(false) },
        )
        manager.createNotificationChannels(channels)
        NotificationChannels // touch to ensure ids stay in sync at compile time
    }
}
