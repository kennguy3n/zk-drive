package com.zkdrive.app.data.sharing

import com.zkdrive.app.config.AppConfig
import com.zkdrive.app.data.remote.ZkDriveApi
import com.zkdrive.app.data.remote.dto.CreateGuestInviteRequest
import com.zkdrive.app.data.remote.dto.CreateShareLinkRequest
import com.zkdrive.app.data.remote.dto.GrantPermissionRequest
import com.zkdrive.app.di.IoDispatcher
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.withContext
import javax.inject.Inject
import javax.inject.Singleton

/** A created share link, with the user-facing URL pre-built. */
data class ShareLink(
    val id: String,
    val token: String,
    val url: String,
    val expiresAt: String?,
    val maxDownloads: Int?,
)

/** A pending or accepted guest invite. */
data class GuestInvite(
    val id: String,
    val email: String,
    val role: String,
    val accepted: Boolean,
)

/** An access grant on a resource. */
data class AccessGrant(
    val id: String,
    val granteeType: String,
    val granteeId: String,
    val role: String,
)

/** Resource type discriminators understood by the sharing API. */
object ResourceType {
    const val FILE = "file"
    const val FOLDER = "folder"
}

/** Roles understood by the permission API. */
object ShareRole {
    const val VIEWER = "viewer"
    const val EDITOR = "editor"
    const val ADMIN = "admin"
}

/**
 * Share links (password / expiry / download cap), guest invites by email, and
 * fine-grained permission grants. Share-link URLs are resolved on the public
 * `/share-links/{token}` route the web app serves.
 */
@Singleton
class SharingRepository @Inject constructor(
    private val api: ZkDriveApi,
    private val appConfig: AppConfig,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    suspend fun createShareLink(
        resourceType: String,
        resourceId: String,
        password: String?,
        expiresAt: String?,
        maxDownloads: Int?,
    ): ShareLink = withContext(io) {
        val dto = api.createShareLink(
            CreateShareLinkRequest(
                resourceType = resourceType,
                resourceId = resourceId,
                password = password?.takeIf { it.isNotBlank() },
                expiresAt = expiresAt,
                maxDownloads = maxDownloads,
            ),
        )
        ShareLink(
            id = dto.id,
            token = dto.token,
            url = "${appConfig.shareOrigin}/share/${dto.token}",
            expiresAt = dto.expiresAt,
            maxDownloads = dto.maxDownloads,
        )
    }

    suspend fun revokeShareLink(id: String) = withContext(io) { api.revokeShareLink(id) }

    suspend fun inviteGuest(
        email: String,
        folderId: String,
        role: String,
        expiresAt: String?,
    ): GuestInvite = withContext(io) {
        val dto = api.createGuestInvite(
            CreateGuestInviteRequest(
                email = email,
                folderId = folderId,
                role = role,
                expiresAt = expiresAt,
            ),
        )
        GuestInvite(id = dto.id, email = dto.email, role = dto.role, accepted = dto.acceptedAt != null)
    }

    suspend fun listPermissions(resourceType: String, resourceId: String): List<AccessGrant> =
        withContext(io) {
            api.listPermissions(resourceType, resourceId).permissions.map {
                AccessGrant(it.id, it.granteeType, it.granteeId, it.role)
            }
        }

    suspend fun grantPermission(
        resourceType: String,
        resourceId: String,
        granteeType: String,
        granteeId: String,
        role: String,
    ): AccessGrant = withContext(io) {
        val dto = api.grantPermission(
            GrantPermissionRequest(
                resourceType = resourceType,
                resourceId = resourceId,
                granteeType = granteeType,
                granteeId = granteeId,
                role = role,
            ),
        )
        AccessGrant(dto.id, dto.granteeType, dto.granteeId, dto.role)
    }

    suspend fun revokePermission(id: String) = withContext(io) { api.revokePermission(id) }
}
