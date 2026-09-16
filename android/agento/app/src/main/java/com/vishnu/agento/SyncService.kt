package com.vishnu.agento

import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat

/** Foreground service wrapper so an immediate sync survives background restrictions on modern Android. */
class SyncService : Service() {

    override fun onCreate() {
        super.onCreate()
        SyncNotifications.ensureChannel(this)
        startForegroundCompat()
    }

    /** Promotes the service to foreground immediately, required before doing work on Android 8+. */
    private fun startForegroundCompat() {
        val info = SyncNotifications.foregroundInfo(this)
        ServiceCompat.startForeground(
            this,
            info.notificationId,
            info.notification,
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                android.content.pm.ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC
            } else {
                0
            },
        )
    }

    /** Kicks off one async sync per start request; STICKY so a killed service is recreated for the next trigger. */
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        SyncRunner.runAsync(this) { result ->
            Log.d("SyncService", "sync done: $result")
        }
        return START_STICKY
    }

    /** No binding offered; callers use the start()/stop() helpers below. */
    override fun onBind(intent: Intent?): IBinder? = null

    companion object {
        /** Starts as a foreground service (required for background data-sync on Android 8+). */
        fun start(context: Context) {
            val intent = Intent(context, SyncService::class.java)
            ContextCompat.startForegroundService(context, intent)
        }

        /** Stops a previously started sync service. */
        fun stop(context: Context) {
            context.stopService(Intent(context, SyncService::class.java))
        }
    }
}
