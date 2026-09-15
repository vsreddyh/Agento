package com.vishnu.healthgateway

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Re-arms hourly sync after reboot or update; WorkManager does not persist alone. */
class BootReceiver : BroadcastReceiver() {
    /** Handles boot/update broadcasts; ignores all other intents. */
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED -> {
                HealthGatewayApp.scheduleSync(context)
            }
        }
    }
}
