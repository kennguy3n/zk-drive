package com.zkdrive.app.ui.search

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.zkdrive.app.data.search.SearchHit
import com.zkdrive.app.data.search.SearchRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.launchIn
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import javax.inject.Inject

data class SearchUiState(
    val query: String = "",
    val searching: Boolean = false,
    val results: List<SearchHit> = emptyList(),
    val error: String? = null,
    val hasSearched: Boolean = false,
)

/**
 * Debounced full-text search. The query text drives a 300ms-debounced flow so
 * we issue one request per pause in typing, not per keystroke. Recent queries
 * are surfaced from the MRU list when the box is empty.
 */
@OptIn(FlowPreview::class)
@HiltViewModel
class SearchViewModel @Inject constructor(
    private val searchRepository: SearchRepository,
) : ViewModel() {

    private val _uiState = MutableStateFlow(SearchUiState())
    val uiState = _uiState.asStateFlow()

    val recentSearches = searchRepository.recentSearches.stateIn(
        scope = viewModelScope,
        started = SharingStarted.WhileSubscribed(5_000),
        initialValue = emptyList(),
    )

    private val queryFlow = MutableStateFlow("")

    init {
        queryFlow
            .debounce(300)
            .map { it.trim() }
            .distinctUntilChanged()
            .onEach { q ->
                if (q.length >= MIN_QUERY) runSearch(q) else clearResults()
            }
            .launchIn(viewModelScope)
    }

    fun onQueryChange(value: String) {
        _uiState.update { it.copy(query = value) }
        queryFlow.value = value
    }

    fun submitRecent(value: String) {
        _uiState.update { it.copy(query = value) }
        queryFlow.value = value
    }

    fun clearQuery() {
        _uiState.update { it.copy(query = "") }
        queryFlow.value = ""
    }

    private suspend fun runSearch(query: String) {
        _uiState.update { it.copy(searching = true, error = null) }
        runCatching { searchRepository.search(query) }
            .onSuccess { hits ->
                _uiState.update {
                    it.copy(searching = false, results = hits, hasSearched = true)
                }
                searchRepository.remember(query)
            }
            .onFailure { e ->
                _uiState.update { it.copy(searching = false, error = e.message, hasSearched = true) }
            }
    }

    private fun clearResults() {
        _uiState.update { it.copy(searching = false, results = emptyList(), hasSearched = false) }
    }

    private companion object {
        const val MIN_QUERY = 2
    }
}
