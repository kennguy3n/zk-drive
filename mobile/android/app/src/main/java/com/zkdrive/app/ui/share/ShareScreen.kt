package com.zkdrive.app.ui.share

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.outlined.ContentCopy
import androidx.compose.material.icons.outlined.Share
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.compose.ui.res.stringResource
import com.zkdrive.app.R
import com.zkdrive.app.data.sharing.ShareRole

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ShareScreen(
    onBack: () -> Unit,
    viewModel: ShareViewModel,
) {
    val state by viewModel.uiState.collectAsStateWithLifecycle()
    val context = LocalContext.current
    val snackbar = remember { SnackbarHostState() }

    LaunchedEffect(state.message) {
        state.message?.let { snackbar.showSnackbar(it); viewModel.consumeMessage() }
    }
    LaunchedEffect(state.error) {
        state.error?.let { snackbar.showSnackbar(it); viewModel.consumeError() }
    }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = { Text("Share \"${state.resourceName}\"", maxLines = 1) },
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(Icons.AutoMirrored.Outlined.ArrowBack, contentDescription = "Back")
                    }
                },
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .padding(padding)
                .padding(16.dp)
                .verticalScroll(rememberScrollState()),
        ) {
            ShareLinkSection(
                state = state,
                onCreate = viewModel::createShareLink,
                onRevoke = viewModel::revokeShareLink,
                onCopy = { copyToClipboard(context, it) },
                onSystemShare = { systemShare(context, it) },
            )

            if (state.isFolder) {
                Spacer(Modifier.height(24.dp))
                HorizontalDivider()
                Spacer(Modifier.height(16.dp))
                GuestInviteSection(busy = state.busy, onInvite = viewModel::inviteGuest)
            }

            Spacer(Modifier.height(24.dp))
            HorizontalDivider()
            Spacer(Modifier.height(16.dp))
            Text("People with access", style = MaterialTheme.typography.titleMedium)
            if (state.permissions.isEmpty()) {
                Text(
                    "No direct grants yet.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(vertical = 8.dp),
                )
            } else {
                state.permissions.forEach { grant ->
                    ListItem(
                        headlineContent = { Text(grant.granteeId, maxLines = 1) },
                        supportingContent = { Text("${grant.granteeType} · ${grant.role}") },
                        trailingContent = {
                            TextButton(onClick = { viewModel.revokePermission(grant) }) { Text("Revoke") }
                        },
                    )
                }
            }
        }
    }
}

@Composable
private fun ShareLinkSection(
    state: ShareUiState,
    onCreate: (password: String?, expiresInDays: Int?, maxDownloads: Int?) -> Unit,
    onRevoke: () -> Unit,
    onCopy: (String) -> Unit,
    onSystemShare: (String) -> Unit,
) {
    var password by remember { mutableStateOf("") }
    var expiryDays by remember { mutableStateOf("") }
    var maxDownloads by remember { mutableStateOf("") }

    Text("Share link", style = MaterialTheme.typography.titleMedium)
    Spacer(Modifier.height(8.dp))

    val link = state.createdLink
    if (link == null) {
        OutlinedTextField(
            value = password,
            onValueChange = { password = it },
            label = { Text("Password (optional)") },
            visualTransformation = PasswordVisualTransformation(),
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Spacer(Modifier.height(8.dp))
        Row {
            OutlinedTextField(
                value = expiryDays,
                onValueChange = { expiryDays = it.filter(Char::isDigit) },
                label = { Text("Expires (days)") },
                keyboardOptions = androidx.compose.foundation.text.KeyboardOptions(keyboardType = KeyboardType.Number),
                singleLine = true,
                modifier = Modifier.weight(1f),
            )
            Spacer(Modifier.height(8.dp))
            Spacer(Modifier.padding(horizontal = 8.dp))
            OutlinedTextField(
                value = maxDownloads,
                onValueChange = { maxDownloads = it.filter(Char::isDigit) },
                label = { Text("Max downloads") },
                keyboardOptions = androidx.compose.foundation.text.KeyboardOptions(keyboardType = KeyboardType.Number),
                singleLine = true,
                modifier = Modifier.weight(1f),
            )
        }
        Spacer(Modifier.height(12.dp))
        Button(
            onClick = {
                onCreate(
                    password.ifBlank { null },
                    expiryDays.toIntOrNull(),
                    maxDownloads.toIntOrNull(),
                )
            },
            enabled = !state.busy,
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Create link") }
    } else {
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(16.dp)) {
                Text(link.url, style = MaterialTheme.typography.bodyMedium)
                Spacer(Modifier.height(12.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(onClick = { onCopy(link.url) }) {
                        Icon(Icons.Outlined.ContentCopy, contentDescription = null)
                        Spacer(Modifier.padding(horizontal = 4.dp))
                        Text("Copy")
                    }
                    Button(onClick = { onSystemShare(link.url) }) {
                        Icon(Icons.Outlined.Share, contentDescription = null)
                        Spacer(Modifier.padding(horizontal = 4.dp))
                        Text(stringResource(R.string.action_share))
                    }
                }
                Spacer(Modifier.height(8.dp))
                TextButton(onClick = onRevoke) { Text("Revoke link") }
            }
        }
    }
}

@Composable
private fun GuestInviteSection(
    busy: Boolean,
    onInvite: (email: String, role: String, expiresInDays: Int?) -> Unit,
) {
    var email by remember { mutableStateOf("") }
    var role by remember { mutableStateOf(ShareRole.VIEWER) }

    Text("Invite a guest", style = MaterialTheme.typography.titleMedium)
    Spacer(Modifier.height(8.dp))
    OutlinedTextField(
        value = email,
        onValueChange = { email = it },
        label = { Text("Email address") },
        keyboardOptions = androidx.compose.foundation.text.KeyboardOptions(keyboardType = KeyboardType.Email),
        singleLine = true,
        modifier = Modifier.fillMaxWidth(),
    )
    Spacer(Modifier.height(8.dp))
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        listOf(ShareRole.VIEWER, ShareRole.EDITOR, ShareRole.ADMIN).forEach { r ->
            FilterChip(
                selected = role == r,
                onClick = { role = r },
                label = { Text(r.replaceFirstChar(Char::uppercase)) },
            )
        }
    }
    Spacer(Modifier.height(12.dp))
    Button(
        onClick = { onInvite(email, role, null) },
        enabled = !busy && email.isNotBlank(),
        modifier = Modifier.fillMaxWidth(),
    ) { Text("Send invite") }
}

private fun copyToClipboard(context: Context, text: String) {
    val clipboard = context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
    clipboard.setPrimaryClip(ClipData.newPlainText("ZK Drive share link", text))
}

private fun systemShare(context: Context, text: String) {
    val intent = Intent(Intent.ACTION_SEND).apply {
        type = "text/plain"
        putExtra(Intent.EXTRA_TEXT, text)
    }
    context.startActivity(Intent.createChooser(intent, "Share link"))
}
