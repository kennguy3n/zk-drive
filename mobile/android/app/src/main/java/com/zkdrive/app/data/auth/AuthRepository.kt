package com.zkdrive.app.data.auth

import android.content.Context
import android.content.Intent
import com.zkdrive.app.bridge.BridgeHolder
import com.zkdrive.app.bridge.BridgeSession
import com.zkdrive.app.config.AppConfig
import com.zkdrive.app.data.remote.dto.WorkspacesEnvelope
import com.zkdrive.app.data.settings.SettingsRepository
import com.zkdrive.app.data.sync.SyncScheduler
import com.zkdrive.app.di.IoDispatcher
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withContext
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.serialization.json.Json
import okhttp3.OkHttpClient
import okhttp3.Request
import uniffi.zk_mobile_bridge.ApiClient
import uniffi.zk_mobile_bridge.SyncEngine
import uniffi.zk_mobile_bridge.TokenManager
import java.io.File
import javax.inject.Inject
import javax.inject.Named
import javax.inject.Singleton

/** Top-level authentication state that drives navigation. */
sealed interface AuthState {
    /** Bootstrapping — deciding where to route. */
    data object Loading : AuthState
    /** No stored session; show the login screen. */
    data object SignedOut : AuthState
    /** Stored session exists but biometric unlock is required. */
    data object Locked : AuthState
    /** Fully authenticated; [profile] is for display. */
    data class Authenticated(val profile: UserProfile, val workspaceId: String) : AuthState
}

/**
 * Owns the authentication lifecycle and the construction/teardown of the
 * native [BridgeSession]. This is the single source of truth for "is the user
 * signed in", consumed by the nav host and every repository (indirectly, via
 * [BridgeHolder]).
 */
@Singleton
class AuthRepository @Inject constructor(
    @ApplicationContext private val context: Context,
    private val appConfig: AppConfig,
    private val tokenStore: TokenStore,
    private val oauth: OAuthService,
    private val bridgeHolder: BridgeHolder,
    private val settings: SettingsRepository,
    private val syncScheduler: SyncScheduler,
    private val json: Json,
    @Named("plain") private val httpClient: OkHttpClient,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    private val _state = MutableStateFlow<AuthState>(AuthState.Loading)
    val state: StateFlow<AuthState> = _state.asStateFlow()

    /** Decide the initial destination at app launch. */
    suspend fun bootstrap() {
        val persisted = tokenStore.load()
        if (persisted == null) {
            _state.value = AuthState.SignedOut
            return
        }
        val biometricRequired = settings.biometricLockEnabled.first()
        if (biometricRequired) {
            _state.value = AuthState.Locked
        } else {
            establishFromPersisted(persisted)
        }
    }

    /** Build the Custom-Tab sign-in intent (suspends on discovery). */
    suspend fun loginIntent(): Intent {
        val config = oauth.resolveConfiguration()
        return oauth.authorizationIntent(config)
    }

    /** Complete sign-in from the redirect intent. */
    suspend fun completeLogin(data: Intent) {
        val result = oauth.exchange(data)
        val (api, tokenManager) = buildAuthedClients(result.tokens, result.tokenEndpoint)
        val workspaceId: String
        try {
            workspaceId = fetchPrimaryWorkspaceId(result.tokens.accessToken)
            val engine = buildSyncEngine(workspaceId, api)
            // install() takes ownership of the native handles from here on.
            bridgeHolder.install(BridgeSession(tokenManager, api, engine, workspaceId))
        } catch (t: Throwable) {
            // The session was never installed, so nothing else will dispose these
            // native handles — close them here to avoid leaking Rust-side objects
            // (UniFFI's JVM Cleaner can't see handles we drop on the floor).
            closeQuietly(api, tokenManager)
            throw t
        }

        tokenStore.save(
            PersistedSession(
                tokens = tokenManager.snapshot() ?: result.tokens,
                idToken = result.idToken,
                workspaceId = workspaceId,
                tokenEndpoint = result.tokenEndpoint,
                profile = result.profile,
            ),
        )
        _state.value = AuthState.Authenticated(result.profile, workspaceId)
    }

    /** After a successful BiometricPrompt, materialise the stored session. */
    suspend fun unlock() {
        val persisted = tokenStore.load() ?: run {
            _state.value = AuthState.SignedOut
            return
        }
        establishFromPersisted(persisted)
    }

    /** Re-establish a session from encrypted storage (no network round-trip). */
    private suspend fun establishFromPersisted(persisted: PersistedSession) {
        val (api, tokenManager) = buildAuthedClients(persisted.tokens, persisted.tokenEndpoint)
        try {
            val engine = buildSyncEngine(persisted.workspaceId, api)
            bridgeHolder.install(BridgeSession(tokenManager, api, engine, persisted.workspaceId))
        } catch (t: Throwable) {
            // Never installed — release the native handles to avoid a leak.
            closeQuietly(api, tokenManager)
            throw t
        }
        _state.value = AuthState.Authenticated(persisted.profile, persisted.workspaceId)
    }

    /** Persist the latest token snapshot (after a transparent refresh). */
    fun persistTokenSnapshot() {
        val session = bridgeHolder.current() ?: return
        session.tokenManager.snapshot()?.let(tokenStore::updateTokens)
    }

    /** Tear everything down and return to the login screen. */
    suspend fun logout() = withContext(io) {
        // Cancel periodic background sync first so no worker wakes up against a
        // torn-down session after the user has signed out.
        syncScheduler.cancel()
        bridgeHolder.clear()
        tokenStore.clear()
        _state.value = AuthState.SignedOut
    }

    /** Close native bridge handles, swallowing teardown errors. */
    private fun closeQuietly(api: ApiClient, tokenManager: TokenManager) {
        runCatching { api.close() }
        runCatching { tokenManager.close() }
    }

    private fun buildAuthedClients(
        tokens: uniffi.zk_mobile_bridge.TokenBundle,
        tokenEndpoint: String,
    ): Pair<ApiClient, TokenManager> {
        val tokenManager = TokenManager(appConfig.oidcClientId, tokenEndpoint)
        tokenManager.setTokens(tokens)
        val api = ApiClient(appConfig.bridgeBaseUrl, tokenManager)
        return api to tokenManager
    }

    private fun buildSyncEngine(workspaceId: String, api: ApiClient): SyncEngine {
        val dir = File(context.filesDir, "catalogue").apply { mkdirs() }
        val cataloguePath = File(dir, "$workspaceId.db").absolutePath
        return SyncEngine(cataloguePath, workspaceId, api)
    }

    /**
     * Resolve the workspace the user lands in. iam-core scopes the bearer to a
     * tenant; the server returns that tenant's workspace(s) — we select the
     * first, which the picker UI can later override.
     */
    private suspend fun fetchPrimaryWorkspaceId(accessToken: String): String = withContext(io) {
        val request = Request.Builder()
            .url(appConfig.restBaseUrl + "workspaces")
            .header("Authorization", "Bearer $accessToken")
            .build()
        httpClient.newCall(request).execute().use { resp ->
            if (!resp.isSuccessful) {
                throw IllegalStateException("Unable to resolve workspace (HTTP ${resp.code})")
            }
            val body = resp.body?.string().orEmpty()
            val envelope = json.decodeFromString(WorkspacesEnvelope.serializer(), body)
            envelope.workspaces.firstOrNull()?.id
                ?: throw IllegalStateException("Account has no accessible workspace")
        }
    }
}
