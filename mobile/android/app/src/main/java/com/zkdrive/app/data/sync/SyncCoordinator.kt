package com.zkdrive.app.data.sync

import com.zkdrive.app.bridge.BridgeHolder
import com.zkdrive.app.data.auth.AuthRepository
import com.zkdrive.app.di.IoDispatcher
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.longOrNull
import uniffi.zk_mobile_bridge.ChangePage
import uniffi.zk_mobile_bridge.ChangeRecord
import uniffi.zk_mobile_bridge.FileEntry
import uniffi.zk_mobile_bridge.SyncStatus
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Drives one sync tick: pull the next changefeed page through the bridge
 * (advancing the durable cursor), reflect each mutation into the local SQLite
 * catalogue so the offline file tree stays current, then persist the refreshed
 * token snapshot.
 *
 * Used by both the periodic [SyncWorker] and foreground refreshes. Re-entrancy
 * is fine: the catalogue is keyed by remote file id and the cursor advances
 * monotonically.
 */
@Singleton
class SyncCoordinator @Inject constructor(
    private val bridgeHolder: BridgeHolder,
    private val authRepository: AuthRepository,
    private val json: Json,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    /** Run sync ticks until the feed is drained. Returns total mutations applied. */
    suspend fun syncNow(maxPages: Int = MAX_PAGES_PER_RUN): Int = withContext(io) {
        val session = bridgeHolder.current() ?: return@withContext 0
        var applied = 0
        var pages = 0
        do {
            val page: ChangePage = session.syncEngine.pollOnce(PAGE_LIMIT)
            page.mutations.forEach { record ->
                applyMutation(session.syncEngine, record)
                applied++
            }
            pages++
        } while (page.hasMore && pages < maxPages)
        authRepository.persistTokenSnapshot()
        applied
    }

    private fun applyMutation(engine: uniffi.zk_mobile_bridge.SyncEngine, record: ChangeRecord) {
        if (record.kind != KIND_FILE) return // folders/permissions/documents: tree metadata only
        when (record.op) {
            OP_DELETE -> {
                if (engine.get(record.resourceId) != null) {
                    engine.setStatus(record.resourceId, SyncStatus.REMOTE_DELETED)
                }
            }
            else -> {
                val existing = engine.get(record.resourceId)
                if (existing != null) {
                    // Known file changed upstream — flag for re-download.
                    engine.setStatus(record.resourceId, SyncStatus.REMOTE_DIRTY)
                } else {
                    engine.upsert(
                        FileEntry(
                            remoteFileId = record.resourceId,
                            remoteVersionId = "",
                            localPath = record.name,
                            sizeBytes = sizeFromMetadata(record.metadataJson),
                            contentHashHex = ZERO_HASH,
                            status = SyncStatus.REMOTE_DIRTY,
                            pinned = false,
                            updatedAtUnixMs = record.occurredAtUnixMs,
                        ),
                    )
                }
            }
        }
    }

    private fun sizeFromMetadata(metadataJson: String?): ULong {
        if (metadataJson.isNullOrBlank()) return 0UL
        return runCatching {
            val obj = json.parseToJsonElement(metadataJson).jsonObject
            (obj["size_bytes"]?.jsonPrimitive?.longOrNull ?: 0L).coerceAtLeast(0L).toULong()
        }.getOrDefault(0UL)
    }

    private companion object {
        const val KIND_FILE = "file"
        const val OP_DELETE = "delete"
        const val PAGE_LIMIT = 200u
        const val MAX_PAGES_PER_RUN = 25
        const val ZERO_HASH = "0000000000000000000000000000000000000000000000000000000000000000"
    }
}
