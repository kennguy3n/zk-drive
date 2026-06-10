package com.zkdrive.app.ui.navigation

import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Folder
import androidx.compose.material.icons.outlined.Search
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.ui.graphics.vector.ImageVector

/** Stable route constants and argument keys for the nav graph. */
object Routes {
    const val BROWSER = "browser"
    const val SEARCH = "search"
    const val SETTINGS = "settings"

    const val ARG_FOLDER_ID = "folderId"
    const val ARG_FOLDER_NAME = "folderName"
    const val BROWSER_PATTERN =
        "$BROWSER?$ARG_FOLDER_ID={$ARG_FOLDER_ID}&$ARG_FOLDER_NAME={$ARG_FOLDER_NAME}"

    const val PREVIEW = "preview"
    const val ARG_FILE_ID = "fileId"
    const val ARG_FILE_NAME = "name"
    const val ARG_FILE_MIME = "mime"
    const val PREVIEW_PATTERN = "$PREVIEW/{$ARG_FILE_ID}?$ARG_FILE_NAME={$ARG_FILE_NAME}&$ARG_FILE_MIME={$ARG_FILE_MIME}"

    const val SHARE = "share"
    const val ARG_RESOURCE_TYPE = "resourceType"
    const val ARG_RESOURCE_ID = "resourceId"
    const val ARG_RESOURCE_NAME = "resourceName"
    const val SHARE_PATTERN = "$SHARE/{$ARG_RESOURCE_TYPE}/{$ARG_RESOURCE_ID}?$ARG_RESOURCE_NAME={$ARG_RESOURCE_NAME}"

    fun browser(folderId: String, folderName: String): String =
        "$BROWSER?$ARG_FOLDER_ID=${encode(folderId)}&$ARG_FOLDER_NAME=${encode(folderName)}"

    fun preview(fileId: String, name: String, mime: String): String =
        "$PREVIEW/$fileId?$ARG_FILE_NAME=${encode(name)}&$ARG_FILE_MIME=${encode(mime)}"

    fun share(resourceType: String, resourceId: String, name: String): String =
        "$SHARE/$resourceType/$resourceId?$ARG_RESOURCE_NAME=${encode(name)}"

    /**
     * Percent-encode a navigation argument. Uses [android.net.Uri.encode] (which
     * emits %20 for spaces, never '+') so it round-trips exactly through Navigation
     * Compose's built-in [android.net.Uri.decode] of string args — no second manual
     * decode is needed (or correct) on the receiving side.
     */
    private fun encode(value: String): String = android.net.Uri.encode(value)
}

/** Resource discriminators shared across sharing + permission routes. */
object ResourceTypes {
    const val FILE = "file"
    const val FOLDER = "folder"
}

/** The three primary bottom-bar destinations. */
enum class TopLevelDestination(
    val route: String,
    val label: String,
    val icon: ImageVector,
) {
    Browser(Routes.BROWSER, "Files", Icons.Outlined.Folder),
    Search(Routes.SEARCH, "Search", Icons.Outlined.Search),
    Settings(Routes.SETTINGS, "Settings", Icons.Outlined.Settings),
}
