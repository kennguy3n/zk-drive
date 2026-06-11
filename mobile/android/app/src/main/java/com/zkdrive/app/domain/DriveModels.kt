package com.zkdrive.app.domain

/** How a folder's contents are encrypted, shown as a badge in the browser. */
enum class EncryptionMode(val wire: String) {
    /** Client-side keys; the server never sees plaintext or keys. */
    ZeroKnowledge("strict_zk"),
    /** Server-managed envelope encryption. */
    Confidential("managed_encrypted"),
    /** Unspecified / inherited. */
    Unknown("");

    companion object {
        fun fromWire(value: String?): EncryptionMode =
            entries.firstOrNull { it.wire == value } ?: Unknown
    }
}

/** A node in the file tree — either a folder or a file. */
sealed interface DriveItem {
    val id: String
    val name: String
}

data class FolderNode(
    override val id: String,
    override val name: String,
    val parentId: String?,
    val path: String,
    val encryptionMode: EncryptionMode,
) : DriveItem

data class FileNode(
    override val id: String,
    override val name: String,
    val folderId: String,
    val sizeBytes: Long,
    val mimeType: String,
    val hasVersion: Boolean,
    val updatedAt: String?,
) : DriveItem {
    val isImage: Boolean get() = mimeType.startsWith("image/")
    val isPdf: Boolean get() = mimeType == "application/pdf"
    val isText: Boolean get() = mimeType.startsWith("text/") ||
        mimeType == "application/json" || mimeType == "application/xml"
}

/** Workspace storage usage, projected for the settings storage bar. */
data class WorkspaceUsage(
    val name: String,
    val usedBytes: Long,
    val quotaBytes: Long,
    val tier: String,
) {
    /** Fraction of quota consumed, clamped to [0, 1]. */
    val fraction: Float
        get() = if (quotaBytes <= 0) 0f else (usedBytes.toFloat() / quotaBytes).coerceIn(0f, 1f)
}

/** One hop in the breadcrumb trail. id == null denotes the workspace root. */
data class Breadcrumb(val id: String?, val name: String)

/** A folder's resolved contents. */
data class FolderContents(
    val folder: FolderNode?,
    val folders: List<FolderNode>,
    val files: List<FileNode>,
)
