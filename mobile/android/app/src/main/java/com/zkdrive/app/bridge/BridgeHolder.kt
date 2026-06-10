package com.zkdrive.app.bridge

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.util.concurrent.atomic.AtomicReference
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Process-wide holder for the active [BridgeSession].
 *
 * The session is created at sign-in and torn down at logout. Both the
 * foreground app (repositories, view models) and background workers
 * (WorkManager) resolve the live session through this holder, so there is a
 * single owner of the native handles and exactly one place that disposes
 * them. Reads are lock-free; swaps are atomic.
 */
@Singleton
class BridgeHolder @Inject constructor() {

    private val ref = AtomicReference<BridgeSession?>(null)
    private val _state = MutableStateFlow(false)

    /** Emits true while a session is established. */
    val isEstablished: StateFlow<Boolean> = _state.asStateFlow()

    /** Install a new session, disposing any previous one. */
    fun install(session: BridgeSession) {
        ref.getAndSet(session)?.close()
        _state.value = true
    }

    /** The current session, or null when signed out. */
    fun current(): BridgeSession? = ref.get()

    /** The current session or throw — for call sites that require auth. */
    fun require(): BridgeSession =
        ref.get() ?: throw IllegalStateException("No active ZK Drive session")

    /** Dispose and clear the session (logout). */
    fun clear() {
        ref.getAndSet(null)?.close()
        _state.value = false
    }
}
