package com.zkdrive.app.config

import com.zkdrive.app.BuildConfig

/**
 * Resolved runtime configuration for the app.
 *
 * Values come from [BuildConfig], which is itself fed by `oauth.properties`
 * (developer machines) or CI environment variables — never hard-coded secrets.
 */
data class AppConfig(
    val issuer: String,
    val oidcClientId: String,
    val oidcScope: String,
    val redirectUri: String,
    val apiBaseUrl: String,
    val webBaseUrl: String,
) {
    /** Base for the bridge `ApiClient` (server root, no trailing slash). */
    val bridgeBaseUrl: String get() = apiBaseUrl.trimEnd('/')

    /** Public web origin that serves the `/share/{token}` landing page. */
    val shareOrigin: String get() = webBaseUrl.trimEnd('/')

    /** Base for Retrofit (`/api/` prefix, with trailing slash for Url.join). */
    val restBaseUrl: String get() = bridgeBaseUrl + "/api/"

    /** OIDC discovery document. */
    val discoveryUrl: String get() = issuer.trimEnd('/') + "/.well-known/openid-configuration"

    companion object {
        fun fromBuildConfig() = AppConfig(
            issuer = BuildConfig.OIDC_ISSUER,
            oidcClientId = BuildConfig.OIDC_CLIENT_ID,
            oidcScope = BuildConfig.OIDC_SCOPE,
            redirectUri = BuildConfig.OIDC_REDIRECT_URI,
            apiBaseUrl = BuildConfig.API_BASE_URL,
            webBaseUrl = BuildConfig.WEB_BASE_URL,
        )
    }
}
