package com.zkdrive.app.data.remote

import com.zkdrive.app.bridge.BridgeHolder
import okhttp3.Interceptor
import okhttp3.Response
import uniffi.zk_mobile_bridge.BridgeException
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Attaches the current access token to every REST request, sourcing it from
 * the bridge [uniffi.zk_mobile_bridge.TokenManager] so refresh logic lives in
 * exactly one place (the Rust auth module) and the REST + transfer clients can
 * never disagree about the active token.
 *
 * `accessToken()` is a blocking native call; it runs on OkHttp's dispatcher
 * thread, never the main thread.
 */
@Singleton
class AuthInterceptor @Inject constructor(
    private val bridgeHolder: BridgeHolder,
) : Interceptor {

    override fun intercept(chain: Interceptor.Chain): Response {
        val request = chain.request()
        val session = bridgeHolder.current()
            ?: return chain.proceed(request) // unauthenticated bootstrap calls

        val token = try {
            session.tokenManager.accessToken()
        } catch (e: BridgeException.Auth) {
            // No valid/refreshable token — let the request go out unauthenticated;
            // the server answers 401 and the UI routes back to login.
            return chain.proceed(request)
        }

        val authed = request.newBuilder()
            .header("Authorization", "Bearer $token")
            .build()
        return chain.proceed(authed)
    }
}
