package com.zkdrive.app.data.auth

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import dagger.hilt.android.qualifiers.ApplicationContext
import uniffi.zk_mobile_bridge.TokenBundle
import javax.inject.Inject
import javax.inject.Singleton

/** The signed-in user's identity claims, surfaced in Settings. */
data class UserProfile(
    val subject: String,
    val email: String?,
    val name: String?,
)

/** Everything needed to silently re-establish a session on next launch. */
data class PersistedSession(
    val tokens: TokenBundle,
    val idToken: String?,
    val workspaceId: String,
    val tokenEndpoint: String,
    val profile: UserProfile,
)

/**
 * Encrypted-at-rest persistence of OAuth tokens + session metadata.
 *
 * Backed by [EncryptedSharedPreferences] (AES-256 GCM values, AES-256
 * SIV keys) with the master key held in the AndroidKeystore (StrongBox when
 * the device offers it). Token material therefore never touches disk in
 * plaintext, and the keystore key is non-exportable.
 */
@Singleton
class TokenStore @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val prefs: SharedPreferences by lazy {
        val masterKey = MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .setRequestStrongBoxBacked(true)
            .build()
        try {
            build(context, masterKey)
        } catch (e: Exception) {
            // A corrupt keystore entry (e.g. after a restore to a new device)
            // is unrecoverable — drop the file and start clean so the user can
            // simply sign in again rather than being hard-locked out.
            context.deleteSharedPreferences(FILE_NAME)
            build(context, masterKey)
        }
    }

    private fun build(context: Context, masterKey: MasterKey): SharedPreferences =
        EncryptedSharedPreferences.create(
            context,
            FILE_NAME,
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )

    fun save(session: PersistedSession) {
        prefs.edit().apply {
            putString(KEY_ACCESS, session.tokens.accessToken)
            putString(KEY_REFRESH, session.tokens.refreshToken)
            putLong(KEY_EXPIRES, session.tokens.expiresAtUnix)
            putString(KEY_SCOPE, session.tokens.scope)
            putString(KEY_ID_TOKEN, session.idToken)
            putString(KEY_WORKSPACE, session.workspaceId)
            putString(KEY_TOKEN_ENDPOINT, session.tokenEndpoint)
            putString(KEY_SUB, session.profile.subject)
            putString(KEY_EMAIL, session.profile.email)
            putString(KEY_NAME, session.profile.name)
        }.apply()
    }

    /** Refreshed token material is written back after a transparent refresh. */
    fun updateTokens(tokens: TokenBundle) {
        prefs.edit()
            .putString(KEY_ACCESS, tokens.accessToken)
            .putString(KEY_REFRESH, tokens.refreshToken)
            .putLong(KEY_EXPIRES, tokens.expiresAtUnix)
            .putString(KEY_SCOPE, tokens.scope)
            .apply()
    }

    fun load(): PersistedSession? {
        val access = prefs.getString(KEY_ACCESS, null) ?: return null
        val workspace = prefs.getString(KEY_WORKSPACE, null) ?: return null
        val tokenEndpoint = prefs.getString(KEY_TOKEN_ENDPOINT, null) ?: return null
        val sub = prefs.getString(KEY_SUB, null) ?: return null
        return PersistedSession(
            tokens = TokenBundle(
                accessToken = access,
                refreshToken = prefs.getString(KEY_REFRESH, "").orEmpty(),
                expiresAtUnix = prefs.getLong(KEY_EXPIRES, 0L),
                scope = prefs.getString(KEY_SCOPE, "").orEmpty(),
            ),
            idToken = prefs.getString(KEY_ID_TOKEN, null),
            workspaceId = workspace,
            tokenEndpoint = tokenEndpoint,
            profile = UserProfile(
                subject = sub,
                email = prefs.getString(KEY_EMAIL, null),
                name = prefs.getString(KEY_NAME, null),
            ),
        )
    }

    fun hasSession(): Boolean = prefs.contains(KEY_ACCESS)

    fun clear() = prefs.edit().clear().apply()

    private companion object {
        const val FILE_NAME = "zk_secure_tokens"
        const val KEY_ACCESS = "access_token"
        const val KEY_REFRESH = "refresh_token"
        const val KEY_EXPIRES = "expires_at"
        const val KEY_SCOPE = "scope"
        const val KEY_ID_TOKEN = "id_token"
        const val KEY_WORKSPACE = "workspace_id"
        const val KEY_TOKEN_ENDPOINT = "token_endpoint"
        const val KEY_SUB = "sub"
        const val KEY_EMAIL = "email"
        const val KEY_NAME = "name"
    }
}
