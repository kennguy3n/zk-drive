package com.zkdrive.app.ui.login

import android.content.Intent
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.zkdrive.app.data.auth.AuthRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.receiveAsFlow
import kotlinx.coroutines.launch
import javax.inject.Inject

/**
 * Drives the login screen: builds the OAuth authorization intent (off the main
 * thread, since discovery is a network call) and completes the code exchange.
 * Biometric unlock for returning users is triggered by the Activity, which
 * then calls [unlock].
 */
@HiltViewModel
class LoginViewModel @Inject constructor(
    private val authRepository: AuthRepository,
) : ViewModel() {

    val authState = authRepository.state

    private val _busy = MutableStateFlow(false)
    val busy = _busy.asStateFlow()

    private val _error = MutableStateFlow<String?>(null)
    val error = _error.asStateFlow()

    private val _intents = Channel<Intent>(Channel.BUFFERED)

    /** Emits authorization intents for the Activity to launch via Custom Tabs. */
    val loginIntents = _intents.receiveAsFlow()

    fun beginLogin() {
        if (_busy.value) return
        viewModelScope.launch {
            _busy.value = true
            _error.value = null
            try {
                _intents.send(authRepository.loginIntent())
            } catch (e: Exception) {
                _error.value = e.message ?: "Unable to start sign-in"
            } finally {
                _busy.value = false
            }
        }
    }

    fun completeLogin(data: Intent) {
        viewModelScope.launch {
            _busy.value = true
            _error.value = null
            try {
                authRepository.completeLogin(data)
            } catch (e: Exception) {
                _error.value = e.message ?: "Sign-in failed"
            } finally {
                _busy.value = false
            }
        }
    }

    /** Called after a successful BiometricPrompt to materialise the session. */
    fun unlock() {
        viewModelScope.launch {
            _busy.value = true
            try {
                authRepository.unlock()
            } catch (e: Exception) {
                _error.value = e.message ?: "Unlock failed"
            } finally {
                _busy.value = false
            }
        }
    }

    fun signOut() {
        viewModelScope.launch { authRepository.logout() }
    }

    fun clearError() {
        _error.value = null
    }
}
