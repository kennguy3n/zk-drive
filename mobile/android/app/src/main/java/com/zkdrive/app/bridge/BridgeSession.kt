package com.zkdrive.app.bridge

import uniffi.zk_mobile_bridge.ApiClient
import uniffi.zk_mobile_bridge.SyncEngine
import uniffi.zk_mobile_bridge.TokenManager

/**
 * The set of native bridge objects bound to one authenticated user + active
 * workspace. Held by [BridgeHolder] for the lifetime of a signed-in session
 * and disposed on logout.
 *
 * @property tokenManager refreshes/returns bearer tokens (shared by the REST
 *   OkHttp interceptor and the bridge ApiClient).
 * @property apiClient mints presigned transfer URLs + drives the changefeed.
 * @property syncEngine SQLite-backed local catalogue + changefeed cursor.
 * @property workspaceId the active workspace the session is scoped to.
 */
class BridgeSession(
    val tokenManager: TokenManager,
    val apiClient: ApiClient,
    val syncEngine: SyncEngine,
    val workspaceId: String,
) {
    /** Release native handles. Safe to call once; idempotent at the holder. */
    fun close() {
        // Order matters: the engine borrows the api client.
        runCatching { syncEngine.close() }
        runCatching { apiClient.close() }
        runCatching { tokenManager.close() }
    }
}
