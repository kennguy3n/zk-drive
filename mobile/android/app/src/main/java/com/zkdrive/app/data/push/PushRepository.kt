package com.zkdrive.app.data.push

import com.zkdrive.app.data.remote.ZkDriveApi
import com.zkdrive.app.data.remote.dto.DeviceTokenRequest
import com.zkdrive.app.di.IoDispatcher
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.withContext
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Registers/unregisters this device's FCM token with the server so the backend
 * can target native push (POST/DELETE /api/push/register-device). Failures are
 * swallowed — push is best-effort and must never break the foreground UX.
 */
@Singleton
class PushRepository @Inject constructor(
    private val api: ZkDriveApi,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    suspend fun register(token: String) = withContext(io) {
        runCatching { api.registerDevice(DeviceTokenRequest(PLATFORM_ANDROID, token)) }
    }

    suspend fun unregister(token: String) = withContext(io) {
        runCatching { api.unregisterDevice(DeviceTokenRequest(PLATFORM_ANDROID, token)) }
    }

    private companion object {
        const val PLATFORM_ANDROID = "android"
    }
}
