package com.zkdrive.app.data.drive

import android.content.Context
import android.net.Uri
import androidx.core.content.FileProvider
import com.zkdrive.app.bridge.BridgeHolder
import com.zkdrive.app.bridge.CryptoProvider
import com.zkdrive.app.di.IoDispatcher
import com.zkdrive.app.domain.EncryptionMode
import com.zkdrive.app.domain.FileNode
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaTypeOrNull
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import uniffi.zk_mobile_bridge.FileEntry
import uniffi.zk_mobile_bridge.SyncStatus
import java.io.File
import java.security.MessageDigest
import javax.inject.Inject
import javax.inject.Named
import javax.inject.Singleton
import kotlin.coroutines.coroutineContext

/** Bytes + metadata for a queued upload, resolved from a SAF/camera Uri. */
data class UploadInput(
    val displayName: String,
    val mimeType: String,
    val bytes: ByteArray,
)

/** Result of a completed upload. */
data class UploadResult(val fileId: String, val versionId: String, val objectKey: String)

/**
 * Direct-to-storage transfers: encrypt-then-PUT on upload, GET-then-decrypt on
 * download, against presigned URLs minted by the bridge [ApiClient].
 *
 * All byte movement happens on [IoDispatcher]; the crypto seal/open is inline
 * (CPU-bound) but still off the main thread because it runs inside these
 * suspend functions' IO context.
 */
@Singleton
class TransferManager @Inject constructor(
    @ApplicationContext private val context: Context,
    private val bridgeHolder: BridgeHolder,
    private val crypto: CryptoProvider,
    private val keyStore: FileKeyStore,
    @Named("plain") private val httpClient: OkHttpClient,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    /**
     * Seal (when the target folder is strict-ZK) and upload [input] into
     * [folderId], then confirm the version and record it in the local
     * catalogue.
     */
    suspend fun upload(
        folderId: String,
        encryptionMode: EncryptionMode,
        input: UploadInput,
    ): UploadResult = withContext(io) {
        val session = bridgeHolder.require()
        val target = session.apiClient.uploadUrl(folderId, input.displayName, input.mimeType)

        val payload = if (encryptionMode == EncryptionMode.ZeroKnowledge) {
            sealForUpload(session.workspaceId, target.objectKey, input.bytes)
        } else {
            input.bytes
        }
        coroutineContext.ensureActive()

        putObject(target.uploadUrl, input.mimeType, payload)
        val checksum = sha256Hex(payload)
        val versionId = session.apiClient.confirmUpload(
            fileId = target.fileId,
            objectKey = target.objectKey,
            sizeBytes = payload.size.toLong(),
            checksum = checksum,
        )

        session.syncEngine.upsert(
            FileEntry(
                remoteFileId = target.fileId,
                remoteVersionId = versionId,
                localPath = input.displayName,
                sizeBytes = payload.size.toULong(),
                contentHashHex = checksum,
                status = SyncStatus.UP_TO_DATE,
                pinned = false,
                updatedAtUnixMs = System.currentTimeMillis(),
            ),
        )
        UploadResult(target.fileId, versionId, target.objectKey)
    }

    /**
     * Download [file], decrypting if we hold its DEK, and materialise it in
     * the app cache. Returns a content:// Uri sharable with other apps.
     */
    suspend fun downloadToCache(file: FileNode): Uri = withContext(io) {
        val session = bridgeHolder.require()
        val target = session.apiClient.downloadUrl(file.id)
        val raw = getObject(target.downloadUrl)
        coroutineContext.ensureActive()

        val plaintext = keyStore.get(target.objectKey)?.let { key ->
            crypto.engineForObject(
                dek = key.dek(),
                tenantId = key.tenantId,
                bucket = key.bucket,
                objectKeyHashHex = key.objectKeyHashHex,
                versionId = key.versionId,
                convergentNonce = key.convergentNonce,
            ).use { it.decrypt(raw) }
        } ?: raw

        writeToShareCache(file.id, file.name, plaintext)
    }

    private fun sealForUpload(
        workspaceId: String,
        objectKey: String,
        plaintext: ByteArray,
    ): ByteArray {
        val dek = crypto.newDataKey()
        val objectKeyHashHex = sha256Hex(objectKey.toByteArray())
        // The canonical AAD's version component must be known before we encrypt,
        // but the server's version id is only minted by confirmUpload() AFTER the
        // PUT. The presigned objectKey is the unique-per-version storage identity
        // the gateway mints up front (a re-upload of the same fileId yields a new
        // objectKey), so binding to it pins the ciphertext to this specific
        // version — unlike fileId, which is stable across versions and would let
        // an old version's envelope collide with a new one.
        val envelope = EnvelopeKey.of(
            dek = dek,
            tenantId = workspaceId,
            bucket = STORAGE_BUCKET,
            objectKeyHashHex = objectKeyHashHex,
            versionId = objectKey,
            convergentNonce = false,
        )
        val ciphertext = crypto.engineForObject(
            dek = dek,
            tenantId = envelope.tenantId,
            bucket = envelope.bucket,
            objectKeyHashHex = envelope.objectKeyHashHex,
            versionId = envelope.versionId,
            convergentNonce = envelope.convergentNonce,
        ).use { it.encrypt(plaintext) }
        keyStore.put(objectKey, envelope)
        return ciphertext
    }

    private fun putObject(url: String, mimeType: String, body: ByteArray) {
        val media = mimeType.toMediaTypeOrNull()
        val request = Request.Builder()
            .url(url)
            .put(body.toRequestBody(media))
            .build()
        httpClient.newCall(request).execute().use { resp ->
            if (!resp.isSuccessful) {
                throw java.io.IOException("Upload failed (HTTP ${resp.code})")
            }
        }
    }

    private fun getObject(url: String): ByteArray {
        val request = Request.Builder().url(url).get().build()
        httpClient.newCall(request).execute().use { resp ->
            if (!resp.isSuccessful) {
                throw java.io.IOException("Download failed (HTTP ${resp.code})")
            }
            return resp.body?.bytes() ?: ByteArray(0)
        }
    }

    private fun writeToShareCache(fileId: String, name: String, bytes: ByteArray): Uri {
        // Namespace by the immutable file id so two files whose names collapse to
        // the same sanitized form (e.g. "report (1).pdf" and "report_1_.pdf") land
        // in distinct directories instead of silently overwriting one another,
        // while the user-facing share sheet still sees the original display name.
        val dir = File(File(context.cacheDir, "shared"), sanitize(fileId)).apply { mkdirs() }
        val outFile = File(dir, sanitize(name))
        outFile.writeBytes(bytes)
        return FileProvider.getUriForFile(context, "${context.packageName}.fileprovider", outFile)
    }

    private fun sanitize(name: String): String =
        name.replace(Regex("[^A-Za-z0-9._-]"), "_").ifEmpty { "file" }

    private fun sha256Hex(bytes: ByteArray): String =
        MessageDigest.getInstance("SHA-256").digest(bytes)
            .joinToString("") { "%02x".format(it) }

    private companion object {
        // AAD bucket label; binds the envelope to ZK Drive object storage.
        const val STORAGE_BUCKET = "zk-drive"
    }
}
