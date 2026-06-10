package com.zkdrive.app.data.auth

import android.content.Context
import android.content.Intent
import android.net.Uri
import com.zkdrive.app.config.AppConfig
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive
import net.openid.appauth.AuthorizationException
import net.openid.appauth.AuthorizationRequest
import net.openid.appauth.AuthorizationResponse
import net.openid.appauth.AuthorizationService
import net.openid.appauth.AuthorizationServiceConfiguration
import net.openid.appauth.ResponseTypeValues
import uniffi.zk_mobile_bridge.TokenBundle
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

/** Outcome of a successful OAuth code exchange. */
data class AuthResult(
    val tokens: TokenBundle,
    val idToken: String?,
    val tokenEndpoint: String,
    val profile: UserProfile,
)

/**
 * OAuth2 Authorization-Code + PKCE against iam-core, implemented with
 * AppAuth-Android (Chrome Custom Tabs + a deep-link redirect).
 *
 * PKCE is mandatory: AppAuth generates a cryptographically random
 * code_verifier per request and only sends its S256 challenge to the
 * authorize endpoint, so an intercepted authorization code is useless
 * without the verifier held in-process.
 */
@Singleton
class OAuthService @Inject constructor(
    @ApplicationContext private val context: Context,
    private val appConfig: AppConfig,
    private val json: Json,
) {

    /**
     * Resolve the IdP configuration, preferring OIDC discovery and falling
     * back to iam-core's documented endpoints if the discovery document is
     * unreachable.
     */
    suspend fun resolveConfiguration(): AuthorizationServiceConfiguration =
        suspendCancellableCoroutine { cont ->
            AuthorizationServiceConfiguration.fetchFromIssuer(Uri.parse(appConfig.issuer)) { config, ex ->
                when {
                    config != null -> cont.resume(config)
                    else -> cont.resume(manualConfiguration())
                }
            }
        }

    private fun manualConfiguration(): AuthorizationServiceConfiguration {
        val base = appConfig.issuer.trimEnd('/')
        return AuthorizationServiceConfiguration(
            Uri.parse("$base/oauth2/authorize"),
            Uri.parse("$base/oauth2/token"),
        )
    }

    /** Build the intent that launches the Custom Tab sign-in flow. */
    fun authorizationIntent(config: AuthorizationServiceConfiguration): Intent {
        val request = AuthorizationRequest.Builder(
            config,
            appConfig.oidcClientId,
            ResponseTypeValues.CODE,
            Uri.parse(appConfig.redirectUri),
        )
            .setScopes(appConfig.oidcScope.split(" ").filter { it.isNotBlank() })
            .build()
        val service = AuthorizationService(context)
        return service.getAuthorizationRequestIntent(request)
    }

    /**
     * Exchange the authorization-code response carried back on [data] for a
     * token set. Throws [AuthorizationException] on user cancel / IdP error.
     */
    suspend fun exchange(data: Intent): AuthResult {
        val response = AuthorizationResponse.fromIntent(data)
        val error = AuthorizationException.fromIntent(data)
        if (response == null) {
            throw error ?: AuthorizationException.fromTemplate(
                AuthorizationException.GeneralErrors.NETWORK_ERROR,
                IllegalStateException("Empty authorization response"),
            )
        }

        val service = AuthorizationService(context)
        try {
            val tokenResponse = suspendCancellableCoroutine { cont ->
                service.performTokenRequest(response.createTokenExchangeRequest()) { resp, ex ->
                    when {
                        resp != null -> cont.resume(resp)
                        else -> cont.resumeWithException(
                            ex ?: IllegalStateException("Empty token response"),
                        )
                    }
                }
            }

            val access = tokenResponse.accessToken
                ?: throw IllegalStateException("Token response missing access_token")
            val expiresAtSeconds = tokenResponse.accessTokenExpirationTime?.let { it / 1000 } ?: 0L
            val idToken = tokenResponse.idToken

            return AuthResult(
                tokens = TokenBundle(
                    accessToken = access,
                    refreshToken = tokenResponse.refreshToken.orEmpty(),
                    expiresAtUnix = expiresAtSeconds,
                    scope = tokenResponse.scope ?: appConfig.oidcScope,
                ),
                idToken = idToken,
                tokenEndpoint = response.request.configuration.tokenEndpoint.toString(),
                profile = idToken?.let(::parseProfile) ?: UserProfile(access.hashCode().toString(), null, null),
            )
        } finally {
            service.dispose()
        }
    }

    /** Decode the (unverified) id_token payload for display-only claims. */
    private fun parseProfile(idToken: String): UserProfile {
        return try {
            val payload = idToken.split(".").getOrNull(1) ?: return UserProfile("", null, null)
            val decoded = android.util.Base64.decode(payload, android.util.Base64.URL_SAFE or android.util.Base64.NO_PADDING)
            val claims = json.parseToJsonElement(decoded.decodeToString()) as JsonObject
            UserProfile(
                subject = claims["sub"]?.jsonPrimitive?.contentOrNull.orEmpty(),
                email = claims["email"]?.jsonPrimitive?.contentOrNull,
                name = claims["name"]?.jsonPrimitive?.contentOrNull,
            )
        } catch (e: Exception) {
            UserProfile("", null, null)
        }
    }
}
