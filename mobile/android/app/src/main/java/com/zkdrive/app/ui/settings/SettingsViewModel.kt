package com.zkdrive.app.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.zkdrive.app.data.auth.AuthRepository
import com.zkdrive.app.data.auth.AuthState
import com.zkdrive.app.data.drive.DriveRepository
import com.zkdrive.app.data.settings.SettingsRepository
import com.zkdrive.app.data.sync.SyncScheduler
import com.zkdrive.app.domain.WorkspaceUsage
import com.zkdrive.app.push.PushInitializer
import com.zkdrive.app.ui.theme.ThemePreference
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import javax.inject.Inject

data class SettingsPrefs(
    val theme: ThemePreference = ThemePreference.SYSTEM,
    val biometricLock: Boolean = false,
    val notifications: Boolean = true,
    val syncOnWifiOnly: Boolean = true,
    val syncOnChargingOnly: Boolean = false,
)

data class AccountInfo(val name: String, val email: String)

data class SettingsUiState(
    val usage: WorkspaceUsage? = null,
    val error: String? = null,
)

/**
 * Settings: theme, biometric lock, notification opt-in, background-sync
 * constraints, account info, and the workspace storage bar. Preference writes
 * are persisted to DataStore and produce the right side effects — toggling
 * notifications (de)registers the FCM token, and changing sync constraints
 * reschedules the periodic WorkManager job.
 */
@HiltViewModel
class SettingsViewModel @Inject constructor(
    private val settings: SettingsRepository,
    private val driveRepository: DriveRepository,
    private val authRepository: AuthRepository,
    private val syncScheduler: SyncScheduler,
    private val pushInitializer: PushInitializer,
) : ViewModel() {

    val prefs = combine(
        settings.theme,
        settings.biometricLockEnabled,
        settings.notificationsEnabled,
        settings.syncOnWifiOnly,
        settings.syncOnChargingOnly,
    ) { theme, biometric, notifications, wifiOnly, chargingOnly ->
        SettingsPrefs(theme, biometric, notifications, wifiOnly, chargingOnly)
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), SettingsPrefs())

    val account = authRepository.state.map { state ->
        (state as? AuthState.Authenticated)?.profile?.let {
            AccountInfo(name = it.name ?: it.email ?: it.subject, email = it.email ?: "")
        }
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), null)

    private val _uiState = MutableStateFlow(SettingsUiState())
    val uiState = _uiState.asStateFlow()

    init {
        refreshUsage()
    }

    fun refreshUsage() {
        viewModelScope.launch {
            runCatching { driveRepository.workspaceUsage() }
                .onSuccess { usage -> _uiState.update { it.copy(usage = usage, error = null) } }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun setTheme(theme: ThemePreference) = viewModelScope.launch { settings.setTheme(theme) }

    fun setBiometricLock(enabled: Boolean) = viewModelScope.launch { settings.setBiometricLock(enabled) }

    fun setNotifications(enabled: Boolean) = viewModelScope.launch {
        settings.setNotifications(enabled)
        runCatching {
            if (enabled) pushInitializer.registerCurrentToken() else pushInitializer.unregisterCurrentToken()
        }
    }

    fun setSyncOnWifiOnly(enabled: Boolean) = viewModelScope.launch {
        settings.setSyncOnWifiOnly(enabled)
        syncScheduler.reschedule()
    }

    fun setSyncOnChargingOnly(enabled: Boolean) = viewModelScope.launch {
        settings.setSyncOnChargingOnly(enabled)
        syncScheduler.reschedule()
    }

    fun signOut() = viewModelScope.launch {
        runCatching { pushInitializer.unregisterCurrentToken() }
        authRepository.logout()
    }
}
