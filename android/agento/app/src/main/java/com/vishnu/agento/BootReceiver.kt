package com.vishnu.agento

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/**
 * Re-arms all periodic jobs right after a reboot or an update (#366 adds
 * the update poll to the sync + task pair): WorkManager persists them,
 * this avoids waiting for the next window.
 */
class BootReceiver : BroadcastReceiver() {
    /** Holds the broadcast until work finishes; returning early would let
     * the process die mid-fetch after a reboot, silently skipping the
     * widget pull and alarm re-arm. Bounded so a dead server can't hang
     * boot handling forever. */
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    /** Handles boot/update/timezone broadcasts; ignores all others. */
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED -> {
                AgentoApp.scheduleSync(context)
                // WorkManager restores its own schedule across reboots, but
                // an app update clears nothing here and a fresh install has
                // no schedule at all — re-asserting is idempotent either way.
                AgentoApp.scheduleTaskSync(context)
                // Same for the update poll (#366): re-asserted, never doubled.
                // Deliberately only the PERIODIC poll (#409): the startup
                // one-shot is enqueued by AgentoApp.onCreate on the next cold
                // start, which an app update guarantees. Re-arming it here as
                // well would fire a second check for one upgrade, and the
                // debounce would not stop it (the replaced package has just
                // checked nothing). Asymmetry is the fix, not the oversight.
                AgentoApp.scheduleUpdateChecks(context)
                val pending = goAsync()
                scope.launch {
                    try {
                        withTimeoutOrNull(60_000) {
                            // Outcomes ignored: boot has no retry, and the
                            // Deferreds never fail — but await, never join,
                            // so an unexpected throw is observed, not lost.
                            awaitAll(
                                TaskWidget.refresh(context),
                                TaskReminders.refresh(context),
                            )
                        }
                    } finally {
                        pending.finish()
                    }
                }
            }
            // Wall-clock alarms were armed in the old zone: recompute.
            Intent.ACTION_TIMEZONE_CHANGED -> {
                TaskReminders.refresh(context)
            }
        }
    }
}
