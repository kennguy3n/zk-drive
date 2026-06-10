package com.zkdrive.app.push

import android.Manifest
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.content.pm.PackageManager
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import com.zkdrive.app.R
import com.zkdrive.app.data.push.PushRepository
import com.zkdrive.app.di.ApplicationScope
import com.zkdrive.app.ui.MainActivity
import dagger.hilt.android.AndroidEntryPoint
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch
import java.util.concurrent.atomic.AtomicInteger
import javax.inject.Inject

/**
 * Receives FCM device-token rotations and push messages.
 *
 * Token rotations are forwarded to the server so targeting stays current.
 * Data/notification messages are surfaced on the Activity channel and deep-link
 * back into the app when tapped. Inert unless a FirebaseApp was initialised at
 * runtime by [PushInitializer].
 */
@AndroidEntryPoint
class ZkFirebaseMessagingService : FirebaseMessagingService() {

    @Inject lateinit var pushRepository: PushRepository

    @Inject @ApplicationScope
    lateinit var appScope: CoroutineScope

    override fun onNewToken(token: String) {
        appScope.launch { pushRepository.register(token) }
    }

    override fun onMessageReceived(message: RemoteMessage) {
        val title = message.notification?.title
            ?: message.data["title"]
            ?: getString(R.string.app_name)
        val body = message.notification?.body
            ?: message.data["body"]
            ?: return
        postNotification(title, body)
    }

    private fun postNotification(title: String, body: String) {
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
            != PackageManager.PERMISSION_GRANTED
        ) {
            return
        }
        val intent = Intent(this, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP)
        val pending = PendingIntent.getActivity(
            this,
            0,
            intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(this, NotificationChannels.PUSH)
            .setSmallIcon(R.drawable.ic_zk_logo)
            .setContentTitle(title)
            .setContentText(body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(body))
            .setAutoCancel(true)
            .setContentIntent(pending)
            .build()
        // A monotonic id keeps each push as its own notification. body.hashCode()
        // collided (distinct messages with the same hash silently replaced each
        // other) and could even hit 0/negative ids.
        getSystemService(NotificationManager::class.java)
            .notify(nextNotificationId.getAndIncrement(), notification)
    }

    companion object {
        /** Process-wide counter so concurrent pushes get distinct notification ids. */
        private val nextNotificationId = AtomicInteger(1)
    }
}
