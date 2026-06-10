package com.zkdrive.app.bridge

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.getAndUpdate
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Process-wide holder for the active [BridgeSession].
 *
 * The session is created at sign-in and torn down at logout. Both the
 * foreground app (repositories, view models) and background workers
 * (WorkManager) resolve the live session through this holder, so there is a
 * single owner of the native handles and exactly one place that disposes
 * them.
 *
 * The session reference is itself the source of truth: a single
 * [MutableStateFlow] holds it, so "is a session established" can never be
 * observed out of sync with the live handle (there is no separate boolean to
 * drift). Reads are lock-free; swaps are atomic via [getAndUpdate], which also
 * guarantees the previous session is disposed exactly once.
 */
@Singleton
class BridgeHolder @Inject constructor() {

    private val _session = MutableStateFlow<BridgeSession?>(null)

    /** The active session as a stream; null when signed out. */
    val session: StateFlow<BridgeSession?> = _session.asStateFlow()

    /** Install a new session, disposing any previous one. */
    fun install(session: BridgeSession) {
        _session.getAndUpdate { session }?.close()
    }

    /** The current session, or null when signed out. */
    fun current(): BridgeSession? = _session.value

    /** The current session or throw — for call sites that require auth. */
    fun require(): BridgeSession =
        _session.value ?: throw IllegalStateException("No active ZK Drive session")

    /** Dispose and clear the session (logout). */
    fun clear() {
        _session.getAndUpdate { null }?.close()
    }
}
