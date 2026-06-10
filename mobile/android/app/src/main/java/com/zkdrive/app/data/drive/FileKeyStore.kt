package com.zkdrive.app.data.drive

import android.content.Context
import android.content.SharedPreferences
import android.util.Base64
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.SetSerializer
import kotlinx.serialization.builtins.serializer
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
    @ApplicationContext private val context: Context,
    private val json: Json,
) {
    private val masterKey: MasterKey by lazy {
        MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .setRequestStrongBoxBacked(true)
            .build()
    }

    /** objectKey -> sealed [EnvelopeKey]. */
    private val prefs: SharedPreferences by lazy { encryptedPrefs("zk_object_keys") }

    /**
     * fileId -> the set of objectKeys (one per uploaded version) whose DEKs this
     * device holds. Lets [removeForFile] purge every version's key on delete,
     * since the delete flow only knows the fileId, not the per-version objectKeys.
     */
    private val fileIndex: SharedPreferences by lazy { encryptedPrefs("zk_object_key_index") }

    private val indexLock = Any()

    private fun encryptedPrefs(name: String): SharedPreferences =
        EncryptedSharedPreferences.create(
            context,
            name,
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )

    /** Persist [key] for [objectKey] and link it to [fileId] for later cleanup. */
    fun put(fileId: String, objectKey: String, key: EnvelopeKey) {
        // Hold the index lock across BOTH writes so a concurrent removeForFile()
        // can't interleave between the DEK write and the index update and leave
        // an orphaned key (DEK present + indexed) for an already-deleted file.
        synchronized(indexLock) {
            prefs.edit().putString(objectKey, json.encodeToString(EnvelopeKey.serializer(), key)).apply()
            val keys = readIndex(fileId).toMutableSet().apply { add(objectKey) }
            fileIndex.edit().putString(fileId, json.encodeToString(setSerializer, keys)).apply()
        }
    }

    fun get(objectKey: String): EnvelopeKey? =
        prefs.getString(objectKey, null)?.let {
            runCatching { json.decodeFromString(EnvelopeKey.serializer(), it) }.getOrNull()
        }

    fun remove(objectKey: String) = prefs.edit().remove(objectKey).apply()

    /** Drop every DEK held for [fileId] (all versions) plus its index entry. */
    fun removeForFile(fileId: String) {
        synchronized(indexLock) {
            val keys = readIndex(fileId)
            if (keys.isNotEmpty()) {
                prefs.edit().apply { keys.forEach { remove(it) } }.apply()
            }
            fileIndex.edit().remove(fileId).apply()
        }
    }

    private fun readIndex(fileId: String): Set<String> =
        fileIndex.getString(fileId, null)?.let {
            runCatching { json.decodeFromString(setSerializer, it) }.getOrNull()
        } ?: emptySet()

    fun clear() {
        // Hold indexLock for the same reason put()/removeForFile() do: a
        // concurrent put() must not interleave its DEK write and index write
        // around the wipe and leave an index entry pointing at a cleared DEK.
        synchronized(indexLock) {
            prefs.edit().clear().apply()
            fileIndex.edit().clear().apply()
        }
    }

    private companion object {
        private val setSerializer = SetSerializer(String.serializer())
    }
}
