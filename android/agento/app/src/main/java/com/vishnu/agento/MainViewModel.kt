package com.vishnu.agento

import android.app.Application
import androidx.compose.runtime.State
import androidx.compose.runtime.mutableStateOf
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.launch

/** Health-sync UI state; persisted fields live in SharedPreferences, rest is queried. */
data class UiState(
    val serverUrl: String = "",
    val password: String = "",
    val healthAvailable: Boolean = false,
    val healthUpdateRequired: Boolean = false,
    val healthPackageInfo: String = "",
    val permissionsGranted: Boolean = false,
    val syncing: Boolean = false,
    val lastResult: String = "",
    val lastSyncAt: String = "",
)

/** Owns health-sync config and status; chat state lives in per-tab ChatViewModels. */
class MainViewModel(app: Application) : AndroidViewModel(app) {

    private val _state = mutableStateOf(UiState())
    val state: State<UiState> = _state

    init {
        refresh()
    }

    /** Reloads prefs plus Health Connect availability; permission check runs async. */
    fun refresh() {
        val prefs = getApplication<Application>().getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        // Unified server URL first; legacy per-feature keys as fallback so
        // pre-unification configs and backups migrate silently on next save.
        fun url(key: String): String =
            (prefs.getString(key, "") ?: "").trim().trimEnd('/').takeIf { it.isNotEmpty() } ?: ""
        _state.value = _state.value.copy(
            serverUrl = url("server_base_url").ifEmpty { url("server_url").ifEmpty { url("api_base_url") } },
            password = (prefs.getString("app_password", "") ?: "").trim().ifEmpty {
            (prefs.getString("auth_token", "") ?: "").trim()
        },
            lastSyncAt = prefs.getString("last_sync_at", "") ?: "",
        )
        val availability = HealthConnectManager.availabilityStatus(getApplication())
        _state.value = _state.value.copy(
            healthAvailable = availability == HealthConnectAvailability.AVAILABLE,
            healthUpdateRequired = availability == HealthConnectAvailability.UPDATE_REQUIRED,
            healthPackageInfo = HealthConnectManager.healthConnectPackageInfo(getApplication()),
        )
        viewModelScope.launch {
            val granted = try {
                HealthConnectManager(getApplication()).grantedPermissions()
            } catch (e: Exception) {
                emptySet()
            }
            _state.value = _state.value.copy(
                permissionsGranted = granted.intersect(HealthConnectManager.PERMISSIONS).isNotEmpty(),
            )
        }
    }

    /** Updates draft server URL in memory; persisted by Save chat config. */
    fun onServerUrl(v: String) {
        _state.value = _state.value.copy(serverUrl = v)
    }

    /** Updates draft password in memory; persisted by Save chat config. */
    fun onPassword(v: String) {
        _state.value = _state.value.copy(password = v)
    }

    /** Surfaces permission/setup failures without starting a sync. */
    fun onSyncError(message: String) {
        _state.value = _state.value.copy(lastResult = "FAILED — $message")
    }

    /** Guards re-entry, stamps last sync time only on success, then refreshes. */
    fun syncNow() {
        if (_state.value.syncing) return
        _state.value = _state.value.copy(syncing = true, lastResult = "syncing…")
        val app = getApplication<Application>()
        SyncRunner.runAsync(app) { result ->
            val prefs = app.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
            if (result.success) {
                prefs.edit().putString("last_sync_at", java.time.Instant.now().toString()).apply()
            }
            _state.value = _state.value.copy(
                syncing = false,
                lastResult = if (result.success) {
                    "OK (${result.statusCode}) — ${result.message.take(80)}"
                } else {
                    "FAILED — ${result.message}"
                },
            )
            refresh()
        }
    }
}
