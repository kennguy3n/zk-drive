package com.zkdrive.app.data.drive

import android.content.Context
import android.content.SharedPreferences
import android.util.Base64
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import javax.inject.Inject
import javax.inject.Singleton

/**
 * The envelope parameters needed to re-open a sealed object: the raw DEK plus
 * the object-context AAD tuple it was bound to.
 */
@Serializable
data class EnvelopeKey(
    val dekB64: String,
    val tenantId: String,
    val bucket: String,
    val objectKeyHashHex: String,
    val versionId: String,
    val convergentNonce: Boolean,
) {
    fun dek(): ByteArray = Base64.decode(dekB64, Base64.NO_WRAP)

    companion object {
        fun of(
            dek: ByteArray,
            tenantId: String,
            bucket: String,
            objectKeyHashHex: String,
            versionId: String,
            convergentNonce: Boolean,
        ) = EnvelopeKey(
            dekB64 = Base64.encodeToString(dek, Base64.NO_WRAP),
            tenantId = tenantId,
            bucket = bucket,
            objectKeyHashHex = objectKeyHashHex,
            versionId = versionId,
            convergentNonce = convergentNonce,
        )
    }
}

/**
 * Encrypted custody of per-object data-encryption keys.
 *
 * Strict-ZK objects are sealed client-side before upload; their DEKs are held
 * here (AES-256-GCM at rest, AndroidKeystore master key) keyed by object key,
 * so the SAME device can transparently decrypt its downloads. Cross-device key
 * escrow is delivered by the server key-wrap surface (out of scope for the
 * native client); until then key custody is device-local by design, which is
 * why these keys are excluded from cloud backup.
 */
@Singleton
class FileKeyStore @Inject constructor(
    @ApplicationContext context: Context,
    private val json: Json,
) {
    private val prefs: SharedPreferences by lazy {
        val masterKey = MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .setRequestStrongBoxBacked(true)
            .build()
        EncryptedSharedPreferences.create(
            context,
            "zk_object_keys",
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    fun put(objectKey: String, key: EnvelopeKey) {
        prefs.edit().putString(objectKey, json.encodeToString(EnvelopeKey.serializer(), key)).apply()
    }

    fun get(objectKey: String): EnvelopeKey? =
        prefs.getString(objectKey, null)?.let {
            runCatching { json.decodeFromString(EnvelopeKey.serializer(), it) }.getOrNull()
        }

    fun remove(objectKey: String) = prefs.edit().remove(objectKey).apply()

    fun clear() = prefs.edit().clear().apply()
}
