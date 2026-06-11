package com.zkdrive.app.ui.preview

import android.content.Context
import android.net.Uri
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.zkdrive.app.data.drive.TransferManager
import com.zkdrive.app.di.IoDispatcher
import com.zkdrive.app.domain.FileNode
import com.zkdrive.app.ui.navigation.Routes
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import javax.inject.Inject

/** What kind of inline preview a downloaded file supports. */
sealed interface PreviewContent {
    data object Loading : PreviewContent
    data class Image(val uri: Uri) : PreviewContent
    data class Pdf(val uri: Uri) : PreviewContent
    data class TextDoc(val text: String, val uri: Uri) : PreviewContent
    data class Unsupported(val uri: Uri) : PreviewContent
    data class Failed(val message: String) : PreviewContent
}

data class PreviewUiState(
    val fileName: String,
    val mimeType: String,
    val content: PreviewContent = PreviewContent.Loading,
)

/**
 * Downloads (and transparently decrypts) a file to the cache, then exposes it
 * for inline rendering: images via Coil, text inline, PDF via PdfRenderer, and
 * anything else via the system viewer / share sheet. The decrypted bytes never
 * leave app-private cache except through an explicit user share.
 */
@HiltViewModel
class PreviewViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    savedStateHandle: SavedStateHandle,
    private val transferManager: TransferManager,
    @IoDispatcher private val io: CoroutineDispatcher,
) : ViewModel() {

    // Navigation Compose already URL-decodes string args before they land in the
    // SavedStateHandle, so we read them as-is (a second manual decode would mangle
    // names containing '+' or '%').
    private val fileId: String = savedStateHandle.get<String>(Routes.ARG_FILE_ID).orEmpty()
    private val fileName: String = savedStateHandle.get<String>(Routes.ARG_FILE_NAME)?.takeIf { it.isNotEmpty() } ?: "File"
    private val mimeType: String = savedStateHandle.get<String>(Routes.ARG_FILE_MIME)?.takeIf { it.isNotEmpty() } ?: "application/octet-stream"

    private val node = FileNode(
        id = fileId,
        name = fileName,
        folderId = "",
        sizeBytes = 0,
        mimeType = mimeType,
        hasVersion = true,
        updatedAt = null,
    )

    private val _uiState = MutableStateFlow(PreviewUiState(fileName, mimeType))
    val uiState = _uiState.asStateFlow()

    init {
        load()
    }

    fun load() {
        _uiState.value = PreviewUiState(fileName, mimeType, PreviewContent.Loading)
        viewModelScope.launch {
            runCatching { transferManager.downloadToCache(node) }
                .onSuccess { uri -> _uiState.value = _uiState.value.copy(content = classify(uri)) }
                .onFailure { e ->
                    _uiState.value = _uiState.value.copy(
                        content = PreviewContent.Failed(e.message ?: "Download failed"),
                    )
                }
        }
    }

    /** The cached content Uri, once downloaded — used for share / open. */
    fun currentUri(): Uri? = when (val c = _uiState.value.content) {
        is PreviewContent.Image -> c.uri
        is PreviewContent.Pdf -> c.uri
        is PreviewContent.TextDoc -> c.uri
        is PreviewContent.Unsupported -> c.uri
        else -> null
    }

    private suspend fun classify(uri: Uri): PreviewContent = withContext(io) {
        when {
            node.isImage -> PreviewContent.Image(uri)
            node.isPdf -> PreviewContent.Pdf(uri)
            node.isText -> PreviewContent.TextDoc(readText(uri), uri)
            else -> PreviewContent.Unsupported(uri)
        }
    }

    private fun readText(uri: Uri): String =
        context.contentResolver.openInputStream(uri)?.use { input ->
            input.readBytes().decodeToString().take(MAX_TEXT_CHARS)
        } ?: ""

    private companion object {
        const val MAX_TEXT_CHARS = 500_000
    }
}
