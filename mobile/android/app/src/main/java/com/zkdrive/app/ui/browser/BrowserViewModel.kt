package com.zkdrive.app.ui.browser

import android.content.Context
import android.net.Uri
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.work.WorkInfo
import androidx.work.WorkManager
import com.zkdrive.app.data.drive.DriveRepository
import com.zkdrive.app.data.drive.UploadEnqueuer
import com.zkdrive.app.data.sync.SyncCoordinator
import com.zkdrive.app.domain.Breadcrumb
import com.zkdrive.app.domain.EncryptionMode
import com.zkdrive.app.domain.FileNode
import com.zkdrive.app.domain.FolderContents
import com.zkdrive.app.domain.FolderNode
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import javax.inject.Inject

/** Grid vs list presentation of the current folder. */
enum class ViewMode { GRID, LIST }

data class BrowserUiState(
    val loading: Boolean = true,
    val refreshing: Boolean = false,
    val error: String? = null,
    val breadcrumbs: List<Breadcrumb> = listOf(Breadcrumb(null, "My Drive")),
    val folders: List<FolderNode> = emptyList(),
    val files: List<FileNode> = emptyList(),
    val currentFolder: FolderNode? = null,
    val currentEncryptionMode: EncryptionMode = EncryptionMode.Unknown,
    val viewMode: ViewMode = ViewMode.LIST,
) {
    val canUpload: Boolean get() = currentFolder != null
    val isEmpty: Boolean get() = folders.isEmpty() && files.isEmpty()
}

/**
 * Folder navigation, view-mode toggle, pull-to-refresh, folder/file CRUD, and
 * background-upload enqueueing. The breadcrumb trail is the navigation stack;
 * the last crumb is the current location (null id == workspace root).
 */
@HiltViewModel
class BrowserViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val driveRepository: DriveRepository,
    private val uploadEnqueuer: UploadEnqueuer,
    private val syncCoordinator: SyncCoordinator,
) : ViewModel() {

    private val _uiState = MutableStateFlow(BrowserUiState())
    val uiState = _uiState.asStateFlow()

    private var lastSucceededUploads = 0

    init {
        loadRoot()
        observeUploads()
    }

    private fun loadRoot() {
        _uiState.update {
            it.copy(loading = true, error = null, breadcrumbs = listOf(Breadcrumb(null, "My Drive")))
        }
        viewModelScope.launch {
            runCatching { driveRepository.listRoot() }
                .onSuccess { contents -> applyContents(contents, EncryptionMode.Unknown) }
                .onFailure { e -> _uiState.update { it.copy(loading = false, error = e.message) } }
        }
    }

    fun openFolder(folder: FolderNode) {
        _uiState.update {
            it.copy(loading = true, error = null, breadcrumbs = it.breadcrumbs + Breadcrumb(folder.id, folder.name))
        }
        viewModelScope.launch { loadFolder(folder.id, folder.encryptionMode) }
    }

    /** Jump to a crumb in the trail (truncating everything below it). */
    fun navigateTo(crumb: Breadcrumb) {
        val crumbs = _uiState.value.breadcrumbs
        val index = crumbs.indexOfFirst { it.id == crumb.id }
        if (index < 0 || index == crumbs.lastIndex) return
        _uiState.update { it.copy(loading = true, breadcrumbs = crumbs.subList(0, index + 1)) }
        viewModelScope.launch {
            if (crumb.id == null) loadRootInline() else loadFolder(crumb.id, EncryptionMode.Unknown)
        }
    }

    /** Handle a system back press. Returns false when already at root. */
    fun onBack(): Boolean {
        val crumbs = _uiState.value.breadcrumbs
        if (crumbs.size <= 1) return false
        navigateTo(crumbs[crumbs.size - 2])
        return true
    }

    fun refresh() {
        _uiState.update { it.copy(refreshing = true) }
        viewModelScope.launch {
            runCatching { syncCoordinator.syncNow() }
            val current = _uiState.value.breadcrumbs.last()
            runCatching {
                if (current.id == null) driveRepository.listRoot()
                else driveRepository.openFolder(current.id)
            }.onSuccess { contents ->
                applyContents(contents, contents.folder?.encryptionMode ?: EncryptionMode.Unknown, refreshing = true)
            }.onFailure { e -> _uiState.update { it.copy(refreshing = false, error = e.message) } }
        }
    }

    fun toggleViewMode() {
        _uiState.update {
            it.copy(viewMode = if (it.viewMode == ViewMode.LIST) ViewMode.GRID else ViewMode.LIST)
        }
    }

    fun createFolder(name: String) {
        val trimmed = name.trim()
        if (trimmed.isEmpty()) return
        val parentId = _uiState.value.currentFolder?.id
        val mode = _uiState.value.currentEncryptionMode
        viewModelScope.launch {
            runCatching { driveRepository.createFolder(parentId, trimmed, mode) }
                .onSuccess { reloadCurrent() }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun deleteFolder(folder: FolderNode) {
        viewModelScope.launch {
            runCatching { driveRepository.deleteFolder(folder.id) }
                .onSuccess { reloadCurrent() }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun deleteFile(file: FileNode) {
        viewModelScope.launch {
            runCatching { driveRepository.deleteFile(file.id) }
                .onSuccess { reloadCurrent() }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun enqueueUpload(uri: Uri) {
        val folder = _uiState.value.currentFolder ?: return
        viewModelScope.launch {
            runCatching { uploadEnqueuer.enqueue(uri, folder.id, folder.encryptionMode) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun clearError() = _uiState.update { it.copy(error = null) }

    private suspend fun loadFolder(folderId: String, fallbackMode: EncryptionMode) {
        runCatching { driveRepository.openFolder(folderId) }
            .onSuccess { contents ->
                applyContents(contents, contents.folder?.encryptionMode ?: fallbackMode)
            }
            .onFailure { e -> _uiState.update { it.copy(loading = false, error = e.message) } }
    }

    private suspend fun loadRootInline() {
        runCatching { driveRepository.listRoot() }
            .onSuccess { contents -> applyContents(contents, EncryptionMode.Unknown) }
            .onFailure { e -> _uiState.update { it.copy(loading = false, error = e.message) } }
    }

    private fun reloadCurrent() {
        val current = _uiState.value.breadcrumbs.last()
        viewModelScope.launch {
            if (current.id == null) loadRootInline() else loadFolder(current.id, _uiState.value.currentEncryptionMode)
        }
    }

    private fun applyContents(
        contents: FolderContents,
        mode: EncryptionMode,
        refreshing: Boolean = false,
    ) {
        _uiState.update {
            it.copy(
                loading = false,
                refreshing = false,
                error = null,
                folders = contents.folders.sortedBy { f -> f.name.lowercase() },
                files = contents.files.sortedBy { f -> f.name.lowercase() },
                currentFolder = contents.folder,
                currentEncryptionMode = contents.folder?.encryptionMode ?: mode,
            )
        }
    }

    private fun observeUploads() {
        viewModelScope.launch {
            WorkManager.getInstance(context)
                .getWorkInfosByTagFlow("zk_upload")
                .collect { infos ->
                    val succeeded = infos.count { it.state == WorkInfo.State.SUCCEEDED }
                    if (succeeded > lastSucceededUploads) {
                        lastSucceededUploads = succeeded
                        reloadCurrent()
                    }
                }
        }
    }
}
