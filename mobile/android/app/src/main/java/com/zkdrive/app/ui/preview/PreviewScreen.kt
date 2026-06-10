package com.zkdrive.app.ui.preview

import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.Color
import android.graphics.pdf.PdfRenderer
import android.net.Uri
import android.os.ParcelFileDescriptor
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.automirrored.outlined.InsertDriveFile
import androidx.compose.material.icons.automirrored.outlined.OpenInNew
import androidx.compose.material.icons.outlined.Share
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.Color as ComposeColor
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import coil.compose.AsyncImage
import com.zkdrive.app.ui.components.ErrorState
import com.zkdrive.app.ui.components.LoadingBox

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PreviewScreen(
    onBack: () -> Unit,
    viewModel: PreviewViewModel,
) {
    val state by viewModel.uiState.collectAsStateWithLifecycle()
    val context = LocalContext.current

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(state.fileName, maxLines = 1) },
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(Icons.AutoMirrored.Outlined.ArrowBack, contentDescription = "Back")
                    }
                },
                actions = {
                    val uri = viewModel.currentUri()
                    if (uri != null) {
                        IconButton(onClick = { shareFile(context, uri, state.mimeType) }) {
                            Icon(Icons.Outlined.Share, contentDescription = "Share")
                        }
                    }
                },
            )
        },
    ) { padding ->
        Box(Modifier.padding(padding).fillMaxSize()) {
            when (val content = state.content) {
                is PreviewContent.Loading -> LoadingBox()
                is PreviewContent.Failed -> ErrorState(content.message, onRetry = viewModel::load)
                is PreviewContent.Image -> AsyncImage(
                    model = content.uri,
                    contentDescription = state.fileName,
                    modifier = Modifier.fillMaxSize(),
                )
                is PreviewContent.TextDoc -> Text(
                    text = content.text,
                    fontFamily = FontFamily.Monospace,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier
                        .fillMaxSize()
                        .verticalScroll(rememberScrollState())
                        .padding(16.dp),
                )
                is PreviewContent.Pdf -> PdfView(content.uri)
                is PreviewContent.Unsupported -> UnsupportedView(
                    onOpen = { openExternally(context, content.uri, state.mimeType) },
                    onShare = { shareFile(context, content.uri, state.mimeType) },
                )
            }
        }
    }
}

@Composable
private fun PdfView(uri: Uri) {
    val context = LocalContext.current
    // Render off the main thread: PdfRenderer + per-page Bitmap rasterisation
    // is heavy CPU/IO and must not block composition.
    var pages by remember(uri) { mutableStateOf<List<Bitmap>?>(null) }
    LaunchedEffect(uri) {
        pages = withContext(Dispatchers.IO) {
            runCatching { renderPdf(context, uri) }.getOrDefault(emptyList())
        }
    }
    when (val bitmaps = pages) {
        null -> LoadingBox()
        else -> if (bitmaps.isEmpty()) {
            ErrorState("Unable to render PDF")
        } else {
            LazyColumn(Modifier.fillMaxSize()) {
                items(bitmaps) { bmp ->
                    Image(
                        bitmap = bmp.asImageBitmap(),
                        contentDescription = null,
                        modifier = Modifier
                            .fillMaxWidth()
                            .padding(8.dp)
                            .background(ComposeColor.White),
                    )
                }
            }
        }
    }
}

@Composable
private fun UnsupportedView(onOpen: () -> Unit, onShare: () -> Unit) {
    Column(
        modifier = Modifier.fillMaxSize().padding(32.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = androidx.compose.foundation.layout.Arrangement.Center,
    ) {
        Icon(
            Icons.AutoMirrored.Outlined.InsertDriveFile,
            contentDescription = null,
            modifier = Modifier.padding(16.dp),
        )
        Text("Preview not available", style = MaterialTheme.typography.titleMedium)
        Text(
            "Open with another app or share the decrypted copy.",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Button(onClick = onOpen, modifier = Modifier.padding(top = 16.dp)) {
            Icon(Icons.AutoMirrored.Outlined.OpenInNew, contentDescription = null)
            Text("  Open")
        }
        Button(onClick = onShare, modifier = Modifier.padding(top = 8.dp)) {
            Icon(Icons.Outlined.Share, contentDescription = null)
            Text("  Share")
        }
    }
}

private fun renderPdf(context: Context, uri: Uri): List<Bitmap> {
    val descriptor: ParcelFileDescriptor =
        context.contentResolver.openFileDescriptor(uri, "r") ?: return emptyList()
    descriptor.use { pfd ->
        PdfRenderer(pfd).use { renderer ->
            val out = ArrayList<Bitmap>(renderer.pageCount)
            for (i in 0 until renderer.pageCount) {
                renderer.openPage(i).use { page ->
                    val scale = 2
                    val bitmap = Bitmap.createBitmap(
                        page.width * scale,
                        page.height * scale,
                        Bitmap.Config.ARGB_8888,
                    )
                    bitmap.eraseColor(Color.WHITE)
                    page.render(bitmap, null, null, PdfRenderer.Page.RENDER_MODE_FOR_DISPLAY)
                    out.add(bitmap)
                }
            }
            return out
        }
    }
}

private fun shareFile(context: Context, uri: Uri, mime: String) {
    val intent = Intent(Intent.ACTION_SEND).apply {
        type = mime
        putExtra(Intent.EXTRA_STREAM, uri)
        addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
    }
    context.startActivity(Intent.createChooser(intent, "Share file"))
}

private fun openExternally(context: Context, uri: Uri, mime: String) {
    val intent = Intent(Intent.ACTION_VIEW).apply {
        setDataAndType(uri, mime)
        addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
    }
    context.startActivity(Intent.createChooser(intent, "Open with"))
}
