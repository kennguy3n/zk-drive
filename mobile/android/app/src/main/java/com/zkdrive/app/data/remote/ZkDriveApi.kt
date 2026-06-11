package com.zkdrive.app.data.remote

import com.zkdrive.app.data.remote.dto.CreateFolderRequest
import com.zkdrive.app.data.remote.dto.CreateGuestInviteRequest
import com.zkdrive.app.data.remote.dto.CreateShareLinkRequest
import com.zkdrive.app.data.remote.dto.DeviceTokenRequest
import com.zkdrive.app.data.remote.dto.FolderContentsDto
import com.zkdrive.app.data.remote.dto.FolderDto
import com.zkdrive.app.data.remote.dto.FoldersEnvelope
import com.zkdrive.app.data.remote.dto.GuestInviteDto
import com.zkdrive.app.data.remote.dto.GrantPermissionRequest
import com.zkdrive.app.data.remote.dto.PermissionDto
import com.zkdrive.app.data.remote.dto.PermissionsEnvelope
import com.zkdrive.app.data.remote.dto.SearchEnvelope
import com.zkdrive.app.data.remote.dto.ShareLinkDto
import com.zkdrive.app.data.remote.dto.WorkspaceDto
import com.zkdrive.app.data.remote.dto.WorkspacesEnvelope
import retrofit2.http.Body
import retrofit2.http.DELETE
import retrofit2.http.GET
import retrofit2.http.HTTP
import retrofit2.http.POST
import retrofit2.http.Path
import retrofit2.http.Query

/**
 * REST surface the bridge `ApiClient` does not cover: workspace + folder
 * navigation, search, sharing, permissions, and native push registration.
 * Transfers (upload/download/preview URLs) and the changefeed go through the
 * Rust bridge instead. Every call is authenticated by [AuthInterceptor].
 */
interface ZkDriveApi {

    @GET("workspaces")
    suspend fun listWorkspaces(): WorkspacesEnvelope

    @GET("workspaces/{id}")
    suspend fun getWorkspace(@Path("id") id: String): WorkspaceDto

    @GET("folders")
    suspend fun listFolders(
        @Query("parent_folder_id") parentFolderId: String?,
    ): FoldersEnvelope

    @GET("folders/{id}")
    suspend fun getFolder(@Path("id") id: String): FolderContentsDto

    @POST("folders")
    suspend fun createFolder(@Body body: CreateFolderRequest): FolderDto

    @DELETE("folders/{id}")
    suspend fun deleteFolder(@Path("id") id: String)

    @DELETE("files/{id}")
    suspend fun deleteFile(@Path("id") id: String)

    @GET("search")
    suspend fun search(
        @Query("q") query: String,
        @Query("limit") limit: Int,
        @Query("offset") offset: Int,
        @Query("fuzzy") fuzzy: Boolean,
    ): SearchEnvelope

    @POST("share-links")
    suspend fun createShareLink(@Body body: CreateShareLinkRequest): ShareLinkDto

    @DELETE("share-links/{id}")
    suspend fun revokeShareLink(@Path("id") id: String)

    @POST("guest-invites")
    suspend fun createGuestInvite(@Body body: CreateGuestInviteRequest): GuestInviteDto

    @GET("permissions")
    suspend fun listPermissions(
        @Query("resource_type") resourceType: String,
        @Query("resource_id") resourceId: String,
    ): PermissionsEnvelope

    @POST("permissions")
    suspend fun grantPermission(@Body body: GrantPermissionRequest): PermissionDto

    @DELETE("permissions/{id}")
    suspend fun revokePermission(@Path("id") id: String)

    @POST("push/register-device")
    suspend fun registerDevice(@Body body: DeviceTokenRequest)

    @HTTP(method = "DELETE", path = "push/register-device", hasBody = true)
    suspend fun unregisterDevice(@Body body: DeviceTokenRequest)
}
