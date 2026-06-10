package com.zkdrive.app.push

import android.content.Context
import com.google.firebase.FirebaseApp
import com.google.firebase.FirebaseOptions
import com.google.firebase.messaging.FirebaseMessaging
import com.zkdrive.app.BuildConfig
import com.zkdrive.app.data.push.PushRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.tasks.await
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Initialises Firebase at runtime from build-injected config instead of the
 * google-services Gradle plugin, so the app builds and runs cleanly without a
 * committed `google-services.json`. When FCM config is absent (e.g. local dev
 * builds), push is simply disabled — never a crash.
 */
@Singleton
class PushInitializer @Inject constructor(
    @ApplicationContext private val context: Context,
    private val pushRepository: PushRepository,
) {
    private val configured: Boolean
        get() = BuildConfig.FCM_PROJECT_ID.isNotBlank() &&
            BuildConfig.FCM_APP_ID.isNotBlank() &&
            BuildConfig.FCM_API_KEY.isNotBlank() &&
            BuildConfig.FCM_SENDER_ID.isNotBlank()

    /** Ensure FirebaseApp exists; safe to call repeatedly. */
    fun ensureInitialized(): Boolean {
        if (!configured) return false
        if (FirebaseApp.getApps(context).isNotEmpty()) return true
        val options = FirebaseOptions.Builder()
            .setProjectId(BuildConfig.FCM_PROJECT_ID)
            .setApplicationId(BuildConfig.FCM_APP_ID)
            .setApiKey(BuildConfig.FCM_API_KEY)
            .setGcmSenderId(BuildConfig.FCM_SENDER_ID)
            .build()
        FirebaseApp.initializeApp(context, options)
        return true
    }

    /** Fetch the current token and register it with the server. */
    suspend fun registerCurrentToken() {
        if (!ensureInitialized()) return
        val token = FirebaseMessaging.getInstance().token.await()
        pushRepository.register(token)
    }

    /** Best-effort unregister + local token delete on logout. */
    suspend fun unregisterCurrentToken() {
        if (!ensureInitialized()) return
        val token = runCatching { FirebaseMessaging.getInstance().token.await() }.getOrNull()
        if (token != null) pushRepository.unregister(token)
        runCatching { FirebaseMessaging.getInstance().deleteToken().await() }
    }
}
