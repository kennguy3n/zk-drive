package com.zkdrive.app.bridge

import uniffi.zk_mobile_bridge.CryptoEngine
import uniffi.zk_mobile_bridge.generateDek
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Thin wrapper over the bridge crypto surface. Keeps the rest of the app from
 * importing UniFFI symbols directly and centralises the
 * "encrypt-before-upload / decrypt-after-download" contract.
 *
 * Crypto runs inline on the caller's thread (it is CPU-bound and fast); the
 * I/O around it is what must move off the main thread, which the repositories
 * enforce with Dispatchers.IO.
 */
@Singleton
class CryptoProvider @Inject constructor() {

    /** Mint a fresh random 32-byte data-encryption key. */
    fun newDataKey(): ByteArray = generateDek()

    /**
     * Build an object-context-bound engine. The context (tenant, bucket,
     * object key hash, version) is mixed into the AEAD additional-data so a
     * ciphertext can only ever be decrypted in the exact location it was
     * sealed — defeating confused-deputy / object-relocation attacks.
     */
    fun engineForObject(
        dek: ByteArray,
        tenantId: String,
        bucket: String,
        objectKeyHashHex: String,
        versionId: String,
        convergentNonce: Boolean = false,
    ): CryptoEngine = CryptoEngine.withObjectContext(
        dek = dek,
        tenantId = tenantId,
        bucket = bucket,
        objectKeyHashHex = objectKeyHashHex,
        versionId = versionId,
        convergentNonce = convergentNonce,
    )

    /** Plain engine bound only to a raw key (no object context). */
    fun engine(dek: ByteArray): CryptoEngine = CryptoEngine(dek)
}
