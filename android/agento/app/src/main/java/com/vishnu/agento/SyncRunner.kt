package com.vishnu.agento

import android.content.Context
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import java.util.concurrent.atomic.AtomicBoolean

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
