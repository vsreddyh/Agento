package com.vishnu.agento

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Re-arms hourly sync right after reboot or update (PeriodicWork persists, this avoids waiting for the next window). */
class BootReceiver : BroadcastReceiver() {
    /** Handles boot/update broadcasts; ignores all other intents. */
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED -> {
                AgentoApp.scheduleSync(context)
                // Re-pull widget data: the collection factory's in-memory
                // cache is empty after process death (review on #116).
                TaskWidget.refresh(context)
                // Alarms don't survive reboot: recompute from open tasks.
                TaskReminders.refresh(context)
            }
        }
    }
}
