package com.zkdrive.app.ui.settings

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.zkdrive.app.domain.WorkspaceUsage
import com.zkdrive.app.ui.theme.ThemePreference
import java.util.Locale

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(viewModel: SettingsViewModel) {
    val prefs by viewModel.prefs.collectAsStateWithLifecycle()
    val account by viewModel.account.collectAsStateWithLifecycle()
    val state by viewModel.uiState.collectAsStateWithLifecycle()

    Scaffold(
        topBar = { TopAppBar(title = { Text("Settings") }) },
    ) { padding ->
        Column(
            modifier = Modifier
                .padding(padding)
                .fillMaxWidth()
                .verticalScroll(rememberScrollState())
                .padding(16.dp),
        ) {
            account?.let { AccountCard(it.name, it.email) }

            Spacer(Modifier.height(16.dp))
            state.usage?.let { StorageCard(it) }

            SectionHeader("Appearance")
            ThemeRow(prefs.theme, viewModel::setTheme)

            SectionHeader("Security")
            ToggleRow(
                title = "Biometric lock",
                subtitle = "Require fingerprint or face unlock to open the app",
                checked = prefs.biometricLock,
                onCheckedChange = viewModel::setBiometricLock,
            )

            SectionHeader("Notifications")
            ToggleRow(
                title = "Push notifications",
                subtitle = "Get notified about shares and sync activity",
                checked = prefs.notifications,
                onCheckedChange = viewModel::setNotifications,
            )

            SectionHeader("Background sync")
            ToggleRow(
                title = "Sync on Wi-Fi only",
                subtitle = "Avoid using mobile data for background sync",
                checked = prefs.syncOnWifiOnly,
                onCheckedChange = viewModel::setSyncOnWifiOnly,
            )
            ToggleRow(
                title = "Sync while charging only",
                subtitle = "Defer background sync until the device is charging",
                checked = prefs.syncOnChargingOnly,
                onCheckedChange = viewModel::setSyncOnChargingOnly,
            )

            Spacer(Modifier.height(24.dp))
            Button(
                onClick = viewModel::signOut,
                modifier = Modifier.fillMaxWidth(),
            ) { Text("Sign out") }
        }
    }
}

@Composable
private fun AccountCard(name: String, email: String) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp)) {
            Text(name, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (email.isNotBlank()) {
                Text(email, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}

@Composable
private fun StorageCard(usage: WorkspaceUsage) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp)) {
            Text("Storage", style = MaterialTheme.typography.titleMedium)
            Spacer(Modifier.height(8.dp))
            LinearProgressIndicator(
                progress = { usage.fraction },
                modifier = Modifier.fillMaxWidth().height(8.dp),
            )
            Spacer(Modifier.height(8.dp))
            Text(
                "${formatBytes(usage.usedBytes)} of ${formatBytes(usage.quotaBytes)} used",
                style = MaterialTheme.typography.bodyMedium,
            )
            if (usage.tier.isNotBlank()) {
                Text(
                    "${usage.tier.replaceFirstChar(Char::uppercase)} plan",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

@Composable
private fun SectionHeader(title: String) {
    Spacer(Modifier.height(20.dp))
    Text(title, style = MaterialTheme.typography.titleSmall, color = MaterialTheme.colorScheme.primary)
    Spacer(Modifier.height(4.dp))
    HorizontalDivider()
}

@Composable
private fun ToggleRow(
    title: String,
    subtitle: String,
    checked: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked = checked, onCheckedChange = onCheckedChange)
    }
}

@Composable
private fun ThemeRow(current: ThemePreference, onSelect: (ThemePreference) -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(vertical = 12.dp)
            .selectableGroup(),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        ThemePreference.entries.forEach { option ->
            FilterChip(
                selected = current == option,
                onClick = { onSelect(option) },
                label = { Text(option.name.lowercase().replaceFirstChar(Char::uppercase)) },
            )
        }
    }
}

private fun formatBytes(bytes: Long): String {
    if (bytes <= 0) return "0 B"
    val units = arrayOf("B", "KB", "MB", "GB", "TB")
    var value = bytes.toDouble()
    var unit = 0
    while (value >= 1024 && unit < units.lastIndex) {
        value /= 1024
        unit++
    }
    return String.format(Locale.US, if (unit == 0) "%.0f %s" else "%.1f %s", value, units[unit])
}
