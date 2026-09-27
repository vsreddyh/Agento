package com.vishnu.agento

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** Re-arms hourly sync right after reboot or update (PeriodicWork persists, this avoids waiting for the next window). */
class BootReceiver : BroadcastReceiver() {
    /** Holds the broadcast until work finishes; returning early would let
     * the process die mid-fetch after a reboot, silently skipping the
     * widget pull and alarm re-arm. Bounded so a dead server can't hang
     * boot handling forever. */
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    /** Handles boot/update broadcasts; ignores all other intents. */
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED -> {
                AgentoApp.scheduleSync(context)
                val pending = goAsync()
                scope.launch {
                    try {
                        withTimeoutOrNull(60_000) {
                            joinAll(
                                TaskWidget.refresh(context),
                                TaskReminders.refresh(context),
                            )
                        }
                    } finally {
                        pending.finish()
                    }
                }
            }
        }
    }
}
