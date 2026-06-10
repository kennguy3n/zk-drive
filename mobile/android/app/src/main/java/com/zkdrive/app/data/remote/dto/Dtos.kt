package com.zkdrive.app.data.remote.dto

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// ---------------------------------------------------------------------------
// Wire DTOs. Field names mirror the Go server JSON tags exactly; the JSON
// decoder is configured with ignoreUnknownKeys so the server can add fields
// without breaking older clients.
// ---------------------------------------------------------------------------

@Serializable
data class WorkspaceDto(
    val id: String,
    val name: String,
    @SerialName("storage_quota_bytes") val storageQuotaBytes: Long = 0,
    @SerialName("storage_used_bytes") val storageUsedBytes: Long = 0,
    val tier: String = "",
)

@Serializable
data class WorkspacesEnvelope(val workspaces: List<WorkspaceDto> = emptyList())

@Serializable
data class FolderDto(
    val id: String,
    @SerialName("workspace_id") val workspaceId: String,
    @SerialName("parent_folder_id") val parentFolderId: String? = null,
    val name: String,
    val path: String = "",
    @SerialName("encryption_mode") val encryptionMode: String = "",
    @SerialName("created_at") val createdAt: String? = null,
    @SerialName("updated_at") val updatedAt: String? = null,
)

@Serializable
data class FileDto(
    val id: String,
    @SerialName("workspace_id") val workspaceId: String,
    @SerialName("folder_id") val folderId: String,
    val name: String,
    @SerialName("current_version_id") val currentVersionId: String? = null,
    @SerialName("size_bytes") val sizeBytes: Long = 0,
    @SerialName("mime_type") val mimeType: String = "",
    @SerialName("created_at") val createdAt: String? = null,
    @SerialName("updated_at") val updatedAt: String? = null,
)

@Serializable
data class FoldersEnvelope(val folders: List<FolderDto> = emptyList())

@Serializable
data class FolderContentsDto(
    val folder: FolderDto,
    val children: List<FolderDto> = emptyList(),
    val files: List<FileDto> = emptyList(),
)

@Serializable
data class CreateFolderRequest(
    @SerialName("workspace_id") val workspaceId: String,
    @SerialName("parent_folder_id") val parentFolderId: String? = null,
    val name: String,
    @SerialName("encryption_mode") val encryptionMode: String? = null,
)

// ----- Search --------------------------------------------------------------

@Serializable
data class SearchResultDto(
    val id: String,
    val type: String,
    val name: String,
    val path: String = "",
    @SerialName("folder_id") val folderId: String? = null,
    val rank: Float = 0f,
    val tags: List<String> = emptyList(),
)

@Serializable
data class SearchEnvelope(
    val hits: List<SearchResultDto> = emptyList(),
    val query: String = "",
    val limit: Int = 0,
    val offset: Int = 0,
    val language: String = "",
    val fuzzy: Boolean = false,
)

// ----- Sharing -------------------------------------------------------------

@Serializable
data class ShareLinkDto(
    val id: String,
    @SerialName("workspace_id") val workspaceId: String,
    @SerialName("resource_type") val resourceType: String,
    @SerialName("resource_id") val resourceId: String,
    val token: String,
    @SerialName("expires_at") val expiresAt: String? = null,
    @SerialName("max_downloads") val maxDownloads: Int? = null,
    @SerialName("download_count") val downloadCount: Int = 0,
    @SerialName("created_at") val createdAt: String? = null,
)

@Serializable
data class CreateShareLinkRequest(
    @SerialName("resource_type") val resourceType: String,
    @SerialName("resource_id") val resourceId: String,
    val password: String? = null,
    @SerialName("expires_at") val expiresAt: String? = null,
    @SerialName("max_downloads") val maxDownloads: Int? = null,
)

@Serializable
data class GuestInviteDto(
    val id: String,
    @SerialName("workspace_id") val workspaceId: String,
    val email: String,
    @SerialName("folder_id") val folderId: String,
    val role: String,
    @SerialName("expires_at") val expiresAt: String? = null,
    @SerialName("accepted_at") val acceptedAt: String? = null,
    @SerialName("permission_id") val permissionId: String? = null,
    @SerialName("created_at") val createdAt: String? = null,
)

@Serializable
data class CreateGuestInviteRequest(
    val email: String,
    @SerialName("folder_id") val folderId: String,
    val role: String,
    @SerialName("expires_at") val expiresAt: String? = null,
)

@Serializable
data class PermissionDto(
    val id: String,
    @SerialName("workspace_id") val workspaceId: String,
    @SerialName("resource_type") val resourceType: String,
    @SerialName("resource_id") val resourceId: String,
    @SerialName("grantee_type") val granteeType: String,
    @SerialName("grantee_id") val granteeId: String,
    val role: String,
)

@Serializable
data class PermissionsEnvelope(val permissions: List<PermissionDto> = emptyList())

@Serializable
data class GrantPermissionRequest(
    @SerialName("resource_type") val resourceType: String,
    @SerialName("resource_id") val resourceId: String,
    @SerialName("grantee_type") val granteeType: String,
    @SerialName("grantee_id") val granteeId: String,
    val role: String,
    @SerialName("expires_at") val expiresAt: String? = null,
)

// ----- Push ----------------------------------------------------------------

@Serializable
data class DeviceTokenRequest(
    val platform: String,
    val token: String,
)
