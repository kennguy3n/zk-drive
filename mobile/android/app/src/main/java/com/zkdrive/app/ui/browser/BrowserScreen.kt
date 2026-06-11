package com.zkdrive.app.ui.browser

import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.InsertDriveFile
import androidx.compose.material.icons.outlined.Add
import androidx.compose.material.icons.outlined.CameraAlt
import androidx.compose.material.icons.outlined.CreateNewFolder
import androidx.compose.material.icons.outlined.Folder
import androidx.compose.material.icons.outlined.GridView
import androidx.compose.material.icons.outlined.Image
import androidx.compose.material.icons.outlined.MoreVert
import androidx.compose.material.icons.outlined.PictureAsPdf
import androidx.compose.material.icons.outlined.Upload
import androidx.compose.material.icons.automirrored.outlined.ViewList
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.core.content.FileProvider
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.zkdrive.app.domain.Breadcrumb
import com.zkdrive.app.domain.DriveItem
import com.zkdrive.app.domain.FileNode
import com.zkdrive.app.domain.FolderNode
import com.zkdrive.app.ui.components.EmptyState
import com.zkdrive.app.ui.components.EncryptionBadge
import com.zkdrive.app.ui.components.ErrorState
import com.zkdrive.app.ui.components.LoadingBox
import com.zkdrive.app.ui.navigation.ResourceTypes
import java.io.File

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun BrowserScreen(
    onOpenFile: (FileNode) -> Unit,
    onShare: (type: String, id: String, name: String) -> Unit,
    viewModel: BrowserViewModel,
) {
    val state by viewModel.uiState.collectAsStateWithLifecycle()
    val context = LocalContext.current
    val snackbar = remember { SnackbarHostState() }

    var showCreateFolder by remember { mutableStateOf(false) }
    var fabExpanded by remember { mutableStateOf(false) }
    var pendingDelete by remember { mutableStateOf<DriveItem?>(null) }
    var cameraTarget by remember { mutableStateOf<Uri?>(null) }

    val documentPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.OpenDocument(),
    ) { uri -> uri?.let(viewModel::enqueueUpload) }

    val cameraLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.TakePicture(),
    ) { success -> if (success) cameraTarget?.let(viewModel::enqueueUpload) }

    LaunchedEffect(state.error) {
        state.error?.let {
            snackbar.showSnackbar(it)
            viewModel.clearError()
        }
    }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = {
                    BreadcrumbBar(
                        breadcrumbs = state.breadcrumbs,
                        onCrumb = viewModel::navigateTo,
                    )
                },
                actions = {
                    IconButton(onClick = viewModel::toggleViewMode) {
                        Icon(
                            imageVector = if (state.viewMode == ViewMode.LIST) {
                                Icons.Outlined.GridView
                            } else {
                                Icons.AutoMirrored.Outlined.ViewList
                            },
                            contentDescription = "Toggle layout",
                        )
                    }
                },
            )
        },
        floatingActionButton = {
            BrowserFab(
                expanded = fabExpanded,
                canUpload = state.canUpload,
                onToggle = { fabExpanded = !fabExpanded },
                onNewFolder = {
                    fabExpanded = false
                    showCreateFolder = true
                },
                onUpload = {
                    fabExpanded = false
                    documentPicker.launch(arrayOf("*/*"))
                },
                onCamera = {
                    fabExpanded = false
                    val uri = newCameraUri(context)
                    cameraTarget = uri
                    cameraLauncher.launch(uri)
                },
            )
        },
    ) { padding ->
        Box(Modifier.padding(padding)) {
            PullToRefreshBox(
                isRefreshing = state.refreshing,
                onRefresh = viewModel::refresh,
                modifier = Modifier.fillMaxSize(),
            ) {
                when {
                    state.loading -> LoadingBox()
                    state.error != null && state.isEmpty ->
                        ErrorState(state.error!!, onRetry = viewModel::refresh)
                    state.isEmpty -> EmptyState(
                        icon = Icons.Outlined.Folder,
                        title = "This folder is empty",
                        caption = if (state.canUpload) {
                            "Tap + to upload files or create a folder"
                        } else {
                            "Tap + to create a folder"
                        },
                    )
                    else -> FolderContentsList(
                        state = state,
                        onOpenFolder = viewModel::openFolder,
                        onOpenFile = onOpenFile,
                        onShare = onShare,
                        onDelete = { pendingDelete = it },
                    )
                }
            }
        }
    }

    if (showCreateFolder) {
        CreateFolderDialog(
            encryptionMode = state.currentEncryptionMode,
            onConfirm = {
                viewModel.createFolder(it)
                showCreateFolder = false
            },
            onDismiss = { showCreateFolder = false },
        )
    }

    pendingDelete?.let { item ->
        ConfirmDeleteDialog(
            name = item.name,
            onConfirm = {
                when (item) {
                    is FolderNode -> viewModel.deleteFolder(item)
                    is FileNode -> viewModel.deleteFile(item)
                }
                pendingDelete = null
            },
            onDismiss = { pendingDelete = null },
        )
    }
}

@Composable
private fun FolderContentsList(
    state: BrowserUiState,
    onOpenFolder: (FolderNode) -> Unit,
    onOpenFile: (FileNode) -> Unit,
    onShare: (String, String, String) -> Unit,
    onDelete: (DriveItem) -> Unit,
) {
    val header: @Composable () -> Unit = {
        if (state.currentFolder != null) {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(horizontal = 16.dp, vertical = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                EncryptionBadge(state.currentEncryptionMode)
            }
        }
    }

    if (state.viewMode == ViewMode.GRID) {
        LazyVerticalGrid(
            columns = GridCells.Adaptive(minSize = 140.dp),
            modifier = Modifier.fillMaxSize(),
            contentPadding = androidx.compose.foundation.layout.PaddingValues(12.dp),
        ) {
            item(span = { androidx.compose.foundation.lazy.grid.GridItemSpan(maxLineSpan) }) { header() }
            items(state.folders, key = { "f-${it.id}" }) { folder ->
                DriveGridCell(
                    icon = Icons.Outlined.Folder,
                    label = folder.name,
                    onClick = { onOpenFolder(folder) },
                    onShare = { onShare(ResourceTypes.FOLDER, folder.id, folder.name) },
                    onDelete = { onDelete(folder) },
                )
            }
            items(state.files, key = { "x-${it.id}" }) { file ->
                DriveGridCell(
                    icon = file.typeIcon(),
                    label = file.name,
                    onClick = { onOpenFile(file) },
                    onShare = { onShare(ResourceTypes.FILE, file.id, file.name) },
                    onDelete = { onDelete(file) },
                )
            }
        }
    } else {
        LazyColumn(Modifier.fillMaxSize()) {
            item { header() }
            items(state.folders, key = { "f-${it.id}" }) { folder ->
                DriveRow(
                    icon = Icons.Outlined.Folder,
                    title = folder.name,
                    subtitle = "Folder",
                    onClick = { onOpenFolder(folder) },
                    onShare = { onShare(ResourceTypes.FOLDER, folder.id, folder.name) },
                    onDelete = { onDelete(folder) },
                )
            }
            items(state.files, key = { "x-${it.id}" }) { file ->
                DriveRow(
                    icon = file.typeIcon(),
                    title = file.name,
                    subtitle = humanSize(file.sizeBytes),
                    onClick = { onOpenFile(file) },
                    onShare = { onShare(ResourceTypes.FILE, file.id, file.name) },
                    onDelete = { onDelete(file) },
                )
            }
        }
    }
}

@Composable
private fun BreadcrumbBar(breadcrumbs: List<Breadcrumb>, onCrumb: (Breadcrumb) -> Unit) {
    val last = breadcrumbs.last()
    Column {
        Text(
            text = last.name,
            style = MaterialTheme.typography.titleLarge,
            fontWeight = FontWeight.SemiBold,
            maxLines = 1,
        )
        if (breadcrumbs.size > 1) {
            Text(
                text = breadcrumbs.dropLast(1).joinToString(" / ") { it.name } + " /",
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                modifier = Modifier.clickable { onCrumb(breadcrumbs[breadcrumbs.size - 2]) },
            )
        }
    }
}

private fun FileNode.typeIcon(): ImageVector = when {
    isImage -> Icons.Outlined.Image
    isPdf -> Icons.Outlined.PictureAsPdf
    else -> Icons.AutoMirrored.Outlined.InsertDriveFile
}

private fun newCameraUri(context: android.content.Context): Uri {
    val dir = File(context.cacheDir, "captures").apply { mkdirs() }
    val file = File(dir, "capture_${System.currentTimeMillis()}.jpg")
    return FileProvider.getUriForFile(context, "${context.packageName}.fileprovider", file)
}

private fun humanSize(bytes: Long): String {
    if (bytes < 1024) return "$bytes B"
    val units = listOf("KB", "MB", "GB", "TB")
    var value = bytes.toDouble() / 1024
    var unit = 0
    while (value >= 1024 && unit < units.lastIndex) {
        value /= 1024
        unit++
    }
    return "%.1f %s".format(value, units[unit])
}
