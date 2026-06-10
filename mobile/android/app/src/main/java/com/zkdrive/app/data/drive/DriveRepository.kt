package com.zkdrive.app.data.drive

import com.zkdrive.app.bridge.BridgeHolder
import com.zkdrive.app.data.remote.ZkDriveApi
import com.zkdrive.app.data.remote.dto.CreateFolderRequest
import com.zkdrive.app.data.remote.dto.FileDto
import com.zkdrive.app.data.remote.dto.FolderDto
import com.zkdrive.app.di.IoDispatcher
import com.zkdrive.app.domain.EncryptionMode
import com.zkdrive.app.domain.FileNode
import com.zkdrive.app.domain.FolderContents
import com.zkdrive.app.domain.FolderNode
import com.zkdrive.app.domain.WorkspaceUsage
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.withContext
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Folder navigation + file/folder lifecycle over the REST surface. Transfers
 * (bytes in/out) live in [TransferManager]; this repository only deals with
 * the metadata tree.
 */
@Singleton
class DriveRepository @Inject constructor(
    private val api: ZkDriveApi,
    private val bridgeHolder: BridgeHolder,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    private val workspaceId: String get() = bridgeHolder.require().workspaceId

    /** Root listing: top-level folders for the active workspace. */
    suspend fun listRoot(): FolderContents = withContext(io) {
        val folders = api.listFolders("root").folders.map { it.toNode() }
        FolderContents(folder = null, folders = folders, files = emptyList())
    }

    /** Open a folder: its metadata, child folders, and files. */
    suspend fun openFolder(folderId: String): FolderContents = withContext(io) {
        val contents = api.getFolder(folderId)
        FolderContents(
            folder = contents.folder.toNode(),
            folders = contents.children.map { it.toNode() },
            files = contents.files.map { it.toNode() },
        )
    }

    suspend fun createFolder(parentId: String?, name: String, mode: EncryptionMode): FolderNode =
        withContext(io) {
            api.createFolder(
                CreateFolderRequest(
                    workspaceId = workspaceId,
                    parentFolderId = parentId,
                    name = name,
                    encryptionMode = mode.takeIf { it != EncryptionMode.Unknown }?.wire,
                ),
            ).toNode()
        }

    /** Current workspace storage usage for the settings storage bar. */
    suspend fun workspaceUsage(): WorkspaceUsage = withContext(io) {
        val dto = api.getWorkspace(workspaceId)
        WorkspaceUsage(
            name = dto.name,
            usedBytes = dto.storageUsedBytes,
            quotaBytes = dto.storageQuotaBytes,
            tier = dto.tier,
        )
    }

    suspend fun deleteFolder(folderId: String) = withContext(io) { api.deleteFolder(folderId) }

    suspend fun deleteFile(fileId: String) = withContext(io) { api.deleteFile(fileId) }

    private fun FolderDto.toNode() = FolderNode(
        id = id,
        name = name,
        parentId = parentFolderId,
        path = path,
        encryptionMode = EncryptionMode.fromWire(encryptionMode),
    )

    private fun FileDto.toNode() = FileNode(
        id = id,
        name = name,
        folderId = folderId,
        sizeBytes = sizeBytes,
        mimeType = mimeType,
        hasVersion = currentVersionId != null,
        updatedAt = updatedAt,
    )
}
