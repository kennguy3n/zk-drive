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
    private val lock = Any()
    private var leases = 0
    private var retired = false

    /**
     * Take a lease that keeps the native handles alive for the duration of an
     * operation. Returns `false` if the session has already been retired (a
     * concurrent logout raced ahead) — in that case callers MUST NOT touch any
     * handle. Pair every successful [acquire] with exactly one [release].
     */
    fun acquire(): Boolean = synchronized(lock) {
        if (retired) {
            false
        } else {
            leases++
            true
        }
    }

    /**
     * Release a lease previously taken with [acquire]. Disposes the native
     * handles if this drains the last lease of an already-retired session.
     */
    fun release() {
        val dispose = synchronized(lock) {
            leases--
            retired && leases == 0
        }
        if (dispose) disposeNative()
    }

    /**
     * Retire the session (logout / session swap). No new leases are granted
     * afterwards. The native handles are disposed immediately when no operation
     * holds a lease; otherwise disposal is deferred to the final [release] so an
     * in-flight transfer or sync never touches a freed Rust handle
     * (use-after-close). Safe to call more than once; idempotent at the holder.
     */
    fun close() {
        val dispose = synchronized(lock) {
            if (retired) return
            retired = true
            leases == 0
        }
        if (dispose) disposeNative()
    }

    private fun disposeNative() {
        // Order matters: the engine borrows the api client.
        runCatching { syncEngine.close() }
        runCatching { apiClient.close() }
        runCatching { tokenManager.close() }
    }
}
