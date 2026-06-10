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
import okhttp3.RequestBody.Companion.asRequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import uniffi.zk_mobile_bridge.FileEntry
import uniffi.zk_mobile_bridge.SyncStatus
import java.io.File
import java.security.MessageDigest
import javax.inject.Inject
import javax.inject.Named
import javax.inject.Singleton
import kotlin.coroutines.coroutineContext

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
     * Seal (when the target folder is strict-ZK) and upload the staged [file]
     * into [folderId], then confirm the version and record it in the local
     * catalogue.
     *
     * Confidential (plaintext) uploads stream straight from disk into the PUT
     * body and are hashed in a streaming pass, so heap stays flat regardless of
     * file size. Strict-ZK uploads must buffer the plaintext + ciphertext in
     * memory because the bridge AEAD is single-shot (no chunked/streaming
     * encrypt); that buffer is bounded by the file size.
     */
    suspend fun upload(
        folderId: String,
        encryptionMode: EncryptionMode,
        file: File,
        displayName: String,
        mimeType: String,
    ): UploadResult = withContext(io) {
        // Lease the session for the whole transfer so a concurrent logout can't
        // dispose the native ApiClient / SyncEngine mid-upload (use-after-close).
        bridgeHolder.withSession { session ->
            val target = session.apiClient.uploadUrl(folderId, displayName, mimeType)

            val checksum: String
            val sizeBytes: Long
            val pendingEnvelope: EnvelopeKey?

            if (encryptionMode == EncryptionMode.ZeroKnowledge) {
                val plaintext = file.readBytes()
                coroutineContext.ensureActive()
                val sealed = sealForUpload(session.workspaceId, target.objectKey, plaintext)
                coroutineContext.ensureActive()
                // The PUT Content-Type MUST equal the mime type the presigned URL was
                // signed with (the server signs with the same value it stores as the
                // file's catalog type); sending application/octet-stream here would
                // fail the S3 signature. The stored object's declared type is
                // irrelevant to ZK clients, which always decrypt the raw bytes.
                putBytes(target.uploadUrl, mimeType, sealed.ciphertext)
                checksum = sha256Hex(sealed.ciphertext)
                sizeBytes = sealed.ciphertext.size.toLong()
                pendingEnvelope = sealed.envelope
            } else {
                checksum = sha256OfFile(file)
                sizeBytes = file.length()
                coroutineContext.ensureActive()
                putFile(target.uploadUrl, mimeType, file)
                pendingEnvelope = null
            }

            val versionId = session.apiClient.confirmUpload(
                fileId = target.fileId,
                objectKey = target.objectKey,
                sizeBytes = sizeBytes,
                checksum = checksum,
            )

            // Persist the DEK only AFTER the version is confirmed. If the PUT or the
            // confirm fails (or the worker is retried) we never wrote a key, so a
            // failed upload can't leave an orphaned DEK that no version references.
            pendingEnvelope?.let { keyStore.put(target.objectKey, it) }

            session.syncEngine.upsert(
                FileEntry(
                    remoteFileId = target.fileId,
                    remoteVersionId = versionId,
                    localPath = displayName,
                    sizeBytes = sizeBytes.toULong(),
                    contentHashHex = checksum,
                    status = SyncStatus.UP_TO_DATE,
                    pinned = false,
                    updatedAtUnixMs = System.currentTimeMillis(),
                ),
            )
            UploadResult(target.fileId, versionId, target.objectKey)
        }
    }

    /**
     * Download [file], decrypting if we hold its DEK, and materialise it in
     * the app cache. Returns a content:// Uri sharable with other apps.
     */
    suspend fun downloadToCache(file: FileNode): Uri = withContext(io) {
        // Lease the session so a concurrent logout can't dispose the native
        // ApiClient while we mint the presigned URL (use-after-close).
        val target = bridgeHolder.withSession { session -> session.apiClient.downloadUrl(file.id) }
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

    /** A sealed payload plus the envelope to persist once the upload confirms. */
    private class SealedUpload(val ciphertext: ByteArray, val envelope: EnvelopeKey)

    private fun sealForUpload(
        workspaceId: String,
        objectKey: String,
        plaintext: ByteArray,
    ): SealedUpload {
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
        return SealedUpload(ciphertext, envelope)
    }

    private fun putBytes(url: String, mimeType: String, body: ByteArray) {
        val media = mimeType.toMediaTypeOrNull()
        val request = Request.Builder()
            .url(url)
            .put(body.toRequestBody(media))
            .build()
        execPut(request)
    }

    private fun putFile(url: String, mimeType: String, file: File) {
        val media = mimeType.toMediaTypeOrNull()
        val request = Request.Builder()
            .url(url)
            .put(file.asRequestBody(media))
            .build()
        execPut(request)
    }

    private fun execPut(request: Request) {
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

    /** Streaming SHA-256 over a file so large uploads aren't read into memory. */
    private fun sha256OfFile(file: File): String {
        val digest = MessageDigest.getInstance("SHA-256")
        file.inputStream().use { input ->
            val buffer = ByteArray(DEFAULT_BUFFER_SIZE)
            while (true) {
                val read = input.read(buffer)
                if (read < 0) break
                digest.update(buffer, 0, read)
            }
        }
        return digest.digest().joinToString("") { "%02x".format(it) }
    }

    private companion object {
        // AAD bucket label; binds the envelope to ZK Drive object storage.
        const val STORAGE_BUCKET = "zk-drive"
    }
}
