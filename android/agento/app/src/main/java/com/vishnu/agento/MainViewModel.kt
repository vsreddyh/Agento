package com.vishnu.agento

import android.app.Application
import androidx.compose.runtime.State
import androidx.compose.runtime.mutableStateOf
import androidx.core.content.edit
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.launch

/** Health-sync UI state; persisted fields live in SharedPreferences, rest is queried. */
data class UiState(
    val serverUrl: String = "",
    val password: String = "",
    val healthAvailable: Boolean = false,
    val healthUpdateRequired: Boolean = false,
    /** SDK available but the client failed to build (#435 review): a
     * transiently dead provider, not a missing one. Settings offers a
     * retry for this, never a permission grant that cannot fix it. */
    val healthTransient: Boolean = false,
    val healthPackageInfo: String = "",
    val permissionsGranted: Boolean = false,
    val syncing: Boolean = false,
    /** A backup export/import is running (#339): the work lives in this
     * ViewModel's scope (it must survive a rotation), so the gate that
     * disables the buttons lives here too — a composition-scoped flag
     * would reset on rotation and let a second export start next to the
     * first. */
    val backupBusy: Boolean = false,
    val lastResult: String = "",
    val lastSyncAt: String = "",
)

/** Owns health-sync config and status for the thin phone client. */
class MainViewModel(app: Application) : AndroidViewModel(app) {

    private val _state = mutableStateOf(UiState())
    val state: State<UiState> = _state

    init {
        refresh()
    }

    /** Marks a backup export/import as running (#339): the flag shares the
     * work's lifetime (both live in this ViewModel), so a rotation cannot
     * let a second export start next to the first. */
    fun setBackupBusy(busy: Boolean) {
        _state.value = _state.value.copy(backupBusy = busy)
    }

    /** Claims the backup slot, or refuses when one is already running
     * (#339): the button's `enabled` state is UI-only, and two taps
     * before the first recomposition would both pass it — two exports
     * would each hold the ~25 MB peak. Returns false when the slot is
     * taken. */
    fun claimBackup(): Boolean {
        if (_state.value.backupBusy) return false
        _state.value = _state.value.copy(backupBusy = true)
        return true
    }

    /** Reloads prefs plus Health Connect availability; permission check runs async. */
    fun refresh() {
        val prefs = getApplication<Application>().getSharedPreferences(
            AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE,
        )
        // Canonical keys only (#433): a config that still holds the retired
        // per-feature keys reads as unconfigured, so Settings shows empty
        // fields and the user re-enters them once instead of the app quietly
        // bridging to whichever stale value happened to win.
        _state.value = _state.value.copy(
            serverUrl = ServerEndpoint.base(prefs),
            password = ServerEndpoint.password(prefs),
            lastSyncAt = prefs.getString("last_sync_at", "") ?: "",
        )
        val availability = HealthConnectManager.availabilityStatus(getApplication())
        _state.value = _state.value.copy(
            healthAvailable = availability == HealthConnectAvailability.AVAILABLE,
            healthUpdateRequired = availability == HealthConnectAvailability.UPDATE_REQUIRED,
            healthPackageInfo = HealthConnectManager.healthConnectPackageInfo(getApplication()),
        )
        viewModelScope.launch {
            val mgr = HealthConnectManager(getApplication())
            val granted = try {
                mgr.grantedPermissions()
            } catch (e: Exception) {
                emptySet()
            }
            _state.value = _state.value.copy(
                permissionsGranted = granted.intersect(HealthConnectManager.PERMISSIONS).isNotEmpty(),
                // SDK available but the client failed to build (#435
                // review): the null client also forces an empty grant set,
                // so without this the section demands permissions for a
                // dead client. [availability] is captured above, same
                // refresh, so the two halves cannot straddle a change.
                healthTransient = !mgr.available &&
                    availability == HealthConnectAvailability.AVAILABLE,
            )
        }
    }

    /** Updates draft server URL in memory; persisted by Save server. */
    fun onServerUrl(v: String) {
        _state.value = _state.value.copy(serverUrl = v)
    }

    /** Updates draft password in memory; persisted by Save server. */
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
                prefs.edit {
                    putString("last_sync_at", java.time.Instant.now().toString())
                }
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
