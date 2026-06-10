package com.zkdrive.app.ui.share

import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.zkdrive.app.data.sharing.AccessGrant
import com.zkdrive.app.data.sharing.GuestInvite
import com.zkdrive.app.data.sharing.ShareLink
import com.zkdrive.app.data.sharing.SharingRepository
import com.zkdrive.app.ui.navigation.ResourceTypes
import com.zkdrive.app.ui.navigation.Routes
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import java.net.URLDecoder
import java.time.Instant
import java.time.temporal.ChronoUnit
import javax.inject.Inject

data class ShareUiState(
    val resourceType: String,
    val resourceId: String,
    val resourceName: String,
    val isFolder: Boolean,
    val busy: Boolean = false,
    val error: String? = null,
    val message: String? = null,
    val createdLink: ShareLink? = null,
    val permissions: List<AccessGrant> = emptyList(),
    val invites: List<GuestInvite> = emptyList(),
)

/**
 * Backs the sharing screen for a single file or folder: share-link creation
 * (optional password / expiry / download cap), guest invites by email
 * (folders only — invites grant folder access), and read/revoke of existing
 * permission grants.
 */
@HiltViewModel
class ShareViewModel @Inject constructor(
    savedStateHandle: SavedStateHandle,
    private val sharingRepository: SharingRepository,
) : ViewModel() {

    private val resourceType: String =
        savedStateHandle.get<String>(Routes.ARG_RESOURCE_TYPE) ?: ResourceTypes.FILE
    private val resourceId: String =
        savedStateHandle.get<String>(Routes.ARG_RESOURCE_ID).orEmpty()
    private val resourceName: String =
        savedStateHandle.get<String>(Routes.ARG_RESOURCE_NAME)
            ?.let { runCatching { URLDecoder.decode(it, Charsets.UTF_8.name()) }.getOrDefault(it) }
            ?: "Item"

    private val _uiState = MutableStateFlow(
        ShareUiState(
            resourceType = resourceType,
            resourceId = resourceId,
            resourceName = resourceName,
            isFolder = resourceType == ResourceTypes.FOLDER,
        ),
    )
    val uiState = _uiState.asStateFlow()

    init {
        loadPermissions()
    }

    private fun loadPermissions() {
        viewModelScope.launch {
            runCatching { sharingRepository.listPermissions(resourceType, resourceId) }
                .onSuccess { grants -> _uiState.update { it.copy(permissions = grants) } }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun createShareLink(password: String?, expiresInDays: Int?, maxDownloads: Int?) {
        viewModelScope.launch {
            _uiState.update { it.copy(busy = true, error = null) }
            val expiresAt = expiresInDays?.let {
                Instant.now().plus(it.toLong(), ChronoUnit.DAYS).toString()
            }
            runCatching {
                sharingRepository.createShareLink(
                    resourceType = resourceType,
                    resourceId = resourceId,
                    password = password,
                    expiresAt = expiresAt,
                    maxDownloads = maxDownloads,
                )
            }.onSuccess { link ->
                _uiState.update { it.copy(busy = false, createdLink = link, message = "Share link created") }
            }.onFailure { e ->
                _uiState.update { it.copy(busy = false, error = e.message) }
            }
        }
    }

    fun revokeShareLink() {
        val link = _uiState.value.createdLink ?: return
        viewModelScope.launch {
            _uiState.update { it.copy(busy = true) }
            runCatching { sharingRepository.revokeShareLink(link.id) }
                .onSuccess { _uiState.update { it.copy(busy = false, createdLink = null, message = "Link revoked") } }
                .onFailure { e -> _uiState.update { it.copy(busy = false, error = e.message) } }
        }
    }

    fun inviteGuest(email: String, role: String, expiresInDays: Int?) {
        val trimmed = email.trim()
        if (trimmed.isEmpty()) return
        viewModelScope.launch {
            _uiState.update { it.copy(busy = true, error = null) }
            val expiresAt = expiresInDays?.let {
                Instant.now().plus(it.toLong(), ChronoUnit.DAYS).toString()
            }
            runCatching { sharingRepository.inviteGuest(trimmed, resourceId, role, expiresAt) }
                .onSuccess { invite ->
                    _uiState.update {
                        it.copy(busy = false, invites = it.invites + invite, message = "Invited $trimmed")
                    }
                    loadPermissions()
                }
                .onFailure { e -> _uiState.update { it.copy(busy = false, error = e.message) } }
        }
    }

    fun revokePermission(grant: AccessGrant) {
        viewModelScope.launch {
            runCatching { sharingRepository.revokePermission(grant.id) }
                .onSuccess {
                    _uiState.update { it.copy(permissions = it.permissions - grant, message = "Access revoked") }
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun consumeMessage() = _uiState.update { it.copy(message = null) }
    fun consumeError() = _uiState.update { it.copy(error = null) }
}
