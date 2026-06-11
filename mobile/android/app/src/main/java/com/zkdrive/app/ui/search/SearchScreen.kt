package com.zkdrive.app.ui.search

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.InsertDriveFile
import androidx.compose.material.icons.outlined.Close
import androidx.compose.material.icons.outlined.Folder
import androidx.compose.material.icons.outlined.History
import androidx.compose.material.icons.outlined.Search
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.foundation.clickable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.zkdrive.app.data.search.SearchHit
import com.zkdrive.app.ui.components.EmptyState

@Composable
fun SearchScreen(
    onOpenResult: (SearchHit) -> Unit,
    viewModel: SearchViewModel,
) {
    val state by viewModel.uiState.collectAsStateWithLifecycle()
    val recents by viewModel.recentSearches.collectAsStateWithLifecycle()

    Scaffold { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding),
        ) {
            OutlinedTextField(
                value = state.query,
                onValueChange = viewModel::onQueryChange,
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(16.dp),
                placeholder = { Text("Search files and folders") },
                leadingIcon = { Icon(Icons.Outlined.Search, contentDescription = null) },
                trailingIcon = {
                    if (state.query.isNotEmpty()) {
                        IconButton(onClick = viewModel::clearQuery) {
                            Icon(Icons.Outlined.Close, contentDescription = "Clear")
                        }
                    }
                },
                singleLine = true,
            )

            Box(Modifier.fillMaxSize()) {
                when {
                    state.searching -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                        CircularProgressIndicator()
                    }
                    state.query.isBlank() -> RecentSearches(recents, viewModel::submitRecent)
                    state.error != null -> EmptyState(
                        icon = Icons.Outlined.Search,
                        title = "Search failed",
                        caption = state.error,
                    )
                    state.hasSearched && state.results.isEmpty() -> EmptyState(
                        icon = Icons.Outlined.Search,
                        title = "No results",
                        caption = "Try a different term",
                    )
                    else -> ResultList(state.results, onOpenResult)
                }
            }
        }
    }
}

@Composable
private fun ResultList(results: List<SearchHit>, onOpen: (SearchHit) -> Unit) {
    LazyColumn(Modifier.fillMaxSize()) {
        items(results, key = { it.type + it.id }) { hit ->
            ListItem(
                modifier = Modifier.clickable { onOpen(hit) },
                headlineContent = { Text(hit.name, maxLines = 1, overflow = TextOverflow.Ellipsis) },
                supportingContent = {
                    if (hit.path.isNotBlank()) {
                        Text(hit.path, style = MaterialTheme.typography.bodySmall, maxLines = 1)
                    }
                },
                leadingContent = {
                    Icon(
                        imageVector = if (hit.isFolder) {
                            Icons.Outlined.Folder
                        } else {
                            Icons.AutoMirrored.Outlined.InsertDriveFile
                        },
                        contentDescription = null,
                    )
                },
            )
        }
    }
}

@Composable
private fun RecentSearches(recents: List<String>, onPick: (String) -> Unit) {
    if (recents.isEmpty()) {
        EmptyState(
            icon = Icons.Outlined.History,
            title = "Search your drive",
            caption = "Recent searches will appear here",
        )
        return
    }
    LazyColumn(Modifier.fillMaxSize()) {
        item {
            Text(
                "Recent",
                style = MaterialTheme.typography.titleSmall,
                modifier = Modifier.padding(16.dp),
            )
        }
        items(recents, key = { it }) { query ->
            ListItem(
                modifier = Modifier.clickable { onPick(query) },
                headlineContent = { Text(query) },
                leadingContent = { Icon(Icons.Outlined.History, contentDescription = null) },
            )
        }
    }
}
