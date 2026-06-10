package com.zkdrive.app.di

import com.jakewharton.retrofit2.converter.kotlinx.serialization.asConverterFactory
import com.zkdrive.app.BuildConfig
import com.zkdrive.app.config.AppConfig
import com.zkdrive.app.data.remote.AuthInterceptor
import com.zkdrive.app.data.remote.ZkDriveApi
import dagger.Module
import dagger.Provides
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import kotlinx.serialization.json.Json
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.logging.HttpLoggingInterceptor
import retrofit2.Retrofit
import java.util.concurrent.TimeUnit
import javax.inject.Named
import javax.inject.Singleton

/**
 * HTTP + REST wiring.
 *
 * Two OkHttp clients exist by design:
 *  - `"plain"`: no auth header. Used for the pre-session workspace bootstrap
 *    and presigned-URL transfers (those URLs are self-authenticating; adding a
 *    bearer would break the S3 signature).
 *  - default (authed): adds the bearer via [AuthInterceptor] for the REST API.
 */
@Module
@InstallIn(SingletonComponent::class)
object NetworkModule {

    private const val TIMEOUT_SECONDS = 30L

    @Provides
    @Singleton
    fun provideLoggingInterceptor(): HttpLoggingInterceptor =
        HttpLoggingInterceptor().apply {
            level = if (BuildConfig.DEBUG) {
                HttpLoggingInterceptor.Level.BASIC
            } else {
                HttpLoggingInterceptor.Level.NONE
            }
        }

    @Provides
    @Singleton
    @Named("plain")
    fun providePlainClient(logging: HttpLoggingInterceptor): OkHttpClient =
        OkHttpClient.Builder()
            .connectTimeout(TIMEOUT_SECONDS, TimeUnit.SECONDS)
            .readTimeout(TIMEOUT_SECONDS, TimeUnit.SECONDS)
            .writeTimeout(TIMEOUT_SECONDS, TimeUnit.SECONDS)
            .addInterceptor(logging)
            .build()

    @Provides
    @Singleton
    fun provideAuthedClient(
        @Named("plain") base: OkHttpClient,
        authInterceptor: AuthInterceptor,
    ): OkHttpClient =
        base.newBuilder()
            .addInterceptor(authInterceptor)
            .build()

    @Provides
    @Singleton
    fun provideRetrofit(client: OkHttpClient, json: Json, appConfig: AppConfig): Retrofit =
        Retrofit.Builder()
            .baseUrl(appConfig.restBaseUrl)
            .client(client)
            .addConverterFactory(json.asConverterFactory("application/json".toMediaType()))
            .build()

    @Provides
    @Singleton
    fun provideZkDriveApi(retrofit: Retrofit): ZkDriveApi =
        retrofit.create(ZkDriveApi::class.java)
}
