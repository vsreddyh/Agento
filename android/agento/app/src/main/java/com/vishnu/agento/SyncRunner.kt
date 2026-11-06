package com.vishnu.agento

import android.content.Context
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import java.util.concurrent.atomic.AtomicBoolean

/** Unavailable-platform wording (#435): top-level and pure so a JVM test
 * can pin that "no Health Connect" never reads as "no permission" or an
 * empty day. */
internal const val HEALTH_CONNECT_UNAVAILABLE_MESSAGE =
    "Health Connect unavailable on this device"

/** Update-required wording (#435 review): the platform exists but the
 * provider needs an update — a different fix from "not installed", so it
 * gets its own line instead of sharing the unavailable one. */
internal const val HEALTH_CONNECT_UPDATE_REQUIRED_MESSAGE =
    "Health Connect needs an update before syncing"

/** Transient-client wording (#435 review): the SDK reports AVAILABLE but
 * the client failed to build (provider IPC crash, transient bind failure).
 * That is a "try again", never a device state — and it keeps the sync card
 * consistent with the settings card, which reads "ready" for the same SDK
 * state. */
internal const val HEALTH_CONNECT_TRANSIENT_MESSAGE =
    "Health Connect isn't responding — try again"

/** Single-flight orchestrator: backfills history once, then sends today-only payloads. */
object SyncRunner {
    private const val TAG = "SyncRunner"
    private val inFlight = AtomicBoolean(false)

    /** Collects from Health Connect and posts; no-ops with a failure result when permissions/config are missing. */
    suspend fun run(context: Context): SyncResult {
        if (!inFlight.compareAndSet(false, true)) {
            return SyncResult(false, null, "sync already in progress")
        }
        return try {
            val manager = HealthConnectManager(context)
            // Absent platform first (#435): without this the null client
            // answers an empty permission set, and the sync reports "not
            // granted" — a permissions story for a device that has no
            // Health Connect at all. Never post an empty payload either:
            // that would look like a day with no data. The message follows
            // the SDK status the settings card gates on (#435 review): an
            // outdated provider names its fix, and an AVAILABLE platform
            // with a dead client reads as transient ("try again"), not as
            // a device state — the settings section offers the same retry
            // for that state instead of a permission grant.
            if (!manager.available) {
                val message = when (HealthConnectManager.availabilityStatus(context)) {
                    HealthConnectAvailability.UPDATE_REQUIRED ->
                        HEALTH_CONNECT_UPDATE_REQUIRED_MESSAGE
                    HealthConnectAvailability.AVAILABLE ->
                        HEALTH_CONNECT_TRANSIENT_MESSAGE
                    else -> HEALTH_CONNECT_UNAVAILABLE_MESSAGE
                }
                SyncResult(false, null, message)
            } else {
                val granted = manager.grantedPermissions()
                if (granted.intersect(HealthConnectManager.PERMISSIONS).isEmpty()) {
                    SyncResult(false, null, "Health Connect permissions not granted")
                } else {
                    val client = SyncClient(context)
                    val prefs = context.getSharedPreferences(
                        AgentoApp.PREFS_NAME, Context.MODE_PRIVATE,
                    )
                    val firstSyncDone = prefs.getBoolean("first_sync_done", false)
                    if (!firstSyncDone) {
                        backfill(client, manager)
                    } else {
                        client.post(manager.collectToday())
                    }
                }
            }
        } catch (e: Exception) {
            Log.e(TAG, "sync failed", e)
            SyncResult(false, null, e.message ?: e.javaClass.simpleName)
        } finally {
            inFlight.set(false)
        }
    }

    /** Uploads each backfill day in order; aborts on first failure so the retry resumes incomplete history. */
    private suspend fun backfill(client: SyncClient, manager: HealthConnectManager): SyncResult {
        val payloads = manager.collectBackfill()
        if (payloads.isEmpty()) {
            client.markFirstSyncDone()
            return SyncResult(true, null, "no historical data to backfill")
        }
        var last: SyncResult = SyncResult(true, null, "")
        for (p in payloads) {
            last = client.post(p)
            if (!last.success) return last
        }
        client.markFirstSyncDone()
        return last
    }

    /** Fire-and-forget entry point for Service/Worker callbacks; result is delivered via onDone. */
    fun runAsync(context: Context, onDone: (SyncResult) -> Unit = {}) {
        CoroutineScope(Dispatchers.IO).launch {
            onDone(run(context))
        }
    }
}
