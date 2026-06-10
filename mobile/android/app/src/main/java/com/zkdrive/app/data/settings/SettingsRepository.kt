package com.zkdrive.app.data.settings

import android.content.Context
import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import com.zkdrive.app.ui.theme.ThemePreference
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map
import javax.inject.Inject
import javax.inject.Singleton

private val Context.dataStore: DataStore<Preferences> by preferencesDataStore(name = "zk_settings")

/**
 * User preferences persisted via Jetpack DataStore: theme, security
 * (biometric lock), notifications, background-sync constraints, and the
 * recent-search MRU list. Plain (non-secret) preferences only — token
 * material lives in [com.zkdrive.app.data.auth.TokenStore].
 */
@Singleton
class SettingsRepository @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    private val store get() = context.dataStore

    val theme: Flow<ThemePreference> = store.data.map { prefs ->
        prefs[KEY_THEME]?.let { runCatching { ThemePreference.valueOf(it) }.getOrNull() }
            ?: ThemePreference.SYSTEM
    }

    val biometricLockEnabled: Flow<Boolean> = store.data.map { it[KEY_BIOMETRIC] ?: false }
    val notificationsEnabled: Flow<Boolean> = store.data.map { it[KEY_NOTIFICATIONS] ?: true }
    val syncOnWifiOnly: Flow<Boolean> = store.data.map { it[KEY_WIFI_ONLY] ?: true }
    val syncOnChargingOnly: Flow<Boolean> = store.data.map { it[KEY_CHARGING_ONLY] ?: false }

    val recentSearches: Flow<List<String>> = store.data.map { prefs ->
        prefs[KEY_RECENT_SEARCHES]
            ?.split(RECENT_DELIMITER)
            ?.filter { it.isNotBlank() }
            ?: emptyList()
    }

    suspend fun setTheme(value: ThemePreference) = store.edit { it[KEY_THEME] = value.name }
    suspend fun setBiometricLock(enabled: Boolean) = store.edit { it[KEY_BIOMETRIC] = enabled }
    suspend fun setNotifications(enabled: Boolean) = store.edit { it[KEY_NOTIFICATIONS] = enabled }
    suspend fun setSyncOnWifiOnly(enabled: Boolean) = store.edit { it[KEY_WIFI_ONLY] = enabled }
    suspend fun setSyncOnChargingOnly(enabled: Boolean) = store.edit { it[KEY_CHARGING_ONLY] = enabled }

    /** Push [query] to the front of the MRU list, de-duplicated and capped. */
    suspend fun addRecentSearch(query: String) {
        val trimmed = query.trim()
        if (trimmed.isEmpty()) return
        store.edit { prefs ->
            val current = prefs[KEY_RECENT_SEARCHES]
                ?.split(RECENT_DELIMITER)
                ?.filter { it.isNotBlank() && !it.equals(trimmed, ignoreCase = true) }
                ?: emptyList()
            val updated = (listOf(trimmed) + current).take(MAX_RECENT)
            prefs[KEY_RECENT_SEARCHES] = updated.joinToString(RECENT_DELIMITER)
        }
    }

    suspend fun clearRecentSearches() = store.edit { it.remove(KEY_RECENT_SEARCHES) }

    private companion object {
        val KEY_THEME = stringPreferencesKey("theme")
        val KEY_BIOMETRIC = booleanPreferencesKey("biometric_lock")
        val KEY_NOTIFICATIONS = booleanPreferencesKey("notifications_enabled")
        val KEY_WIFI_ONLY = booleanPreferencesKey("sync_wifi_only")
        val KEY_CHARGING_ONLY = booleanPreferencesKey("sync_charging_only")
        val KEY_RECENT_SEARCHES = stringPreferencesKey("recent_searches")
        const val RECENT_DELIMITER = "\u0001"
        const val MAX_RECENT = 8
    }
}
