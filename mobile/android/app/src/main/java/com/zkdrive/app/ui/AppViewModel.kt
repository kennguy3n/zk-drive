package com.zkdrive.app.ui

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.zkdrive.app.data.auth.AuthRepository
import com.zkdrive.app.data.settings.SettingsRepository
import com.zkdrive.app.ui.theme.ThemePreference
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

/**
 * Application-scoped state for the root composable: drives the splash → login →
 * app gate from [AuthRepository.state] and the active theme. Session bootstrap
 * (restoring persisted tokens, deciding whether biometric unlock is required)
 * runs once on construction.
 */
@HiltViewModel
class AppViewModel @Inject constructor(
    private val authRepository: AuthRepository,
    settings: SettingsRepository,
) : ViewModel() {

    val authState = authRepository.state

    val theme = settings.theme.stateIn(
        scope = viewModelScope,
        started = SharingStarted.Eagerly,
        initialValue = ThemePreference.SYSTEM,
    )

    init {
        viewModelScope.launch { authRepository.bootstrap() }
    }

    /** Persist the latest token snapshot from the bridge (called on stop). */
    fun persistSession() = authRepository.persistTokenSnapshot()
}
