package com.zkdrive.app.data.search

import com.zkdrive.app.data.remote.ZkDriveApi
import com.zkdrive.app.data.settings.SettingsRepository
import com.zkdrive.app.di.IoDispatcher
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.withContext
import javax.inject.Inject
import javax.inject.Singleton

/** A search hit projected for the UI. */
data class SearchHit(
    val id: String,
    val type: String,
    val name: String,
    val path: String,
    val folderId: String?,
    val tags: List<String>,
) {
    val isFolder: Boolean get() = type == "folder"
}

/**
 * Workspace-scoped full-text search plus the recent-query MRU list. Querying
 * is server-side (Postgres FTS); the MRU is local user state.
 */
@Singleton
class SearchRepository @Inject constructor(
    private val api: ZkDriveApi,
    private val settings: SettingsRepository,
    @IoDispatcher private val io: CoroutineDispatcher,
) {
    val recentSearches: Flow<List<String>> = settings.recentSearches

    suspend fun search(query: String, fuzzy: Boolean = true, limit: Int = 30): List<SearchHit> =
        withContext(io) {
            val response = api.search(query = query, limit = limit, offset = 0, fuzzy = fuzzy)
            response.hits.map {
                SearchHit(
                    id = it.id,
                    type = it.type,
                    name = it.name,
                    path = it.path,
                    folderId = it.folderId,
                    tags = it.tags,
                )
            }
        }

    suspend fun remember(query: String) = settings.addRecentSearch(query)

    suspend fun clearRecent() = settings.clearRecentSearches()
}
