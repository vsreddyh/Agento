package com.vishnu.healthgateway

import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.content.pm.ServiceInfo
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.work.ForegroundInfo

/** Shared low-priority notification bits required for foreground sync (Service and Worker reuse one channel). */
object SyncNotifications {
    private const val TAG = "SyncNotifications"
    private const val CHANNEL_ID = "health_sync"
    private const val NOTIFICATION_ID = 4242

    /** Creates the sync channel once; no-op on later calls and required before posting on Android 8+. */
    fun ensureChannel(context: Context) {
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (nm.getNotificationChannel(CHANNEL_ID) == null) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                context.getString(R.string.channel_name),
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = context.getString(R.string.channel_desc)
            }
            nm.createNotificationChannel(channel)
        }
    }

    /** Builds the ongoing DATA_SYNC ForegroundInfo shared by SyncService and SyncWorker. */
    fun foregroundInfo(context: Context): ForegroundInfo {
        ensureChannel(context)
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setContentTitle(context.getString(R.string.sync_notif_title))
            .setContentText(context.getString(R.string.sync_notif_text))
            .setSmallIcon(android.R.drawable.stat_notify_sync)
            .setOngoing(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
        return ForegroundInfo(
            NOTIFICATION_ID,
            notification,
            ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC,
        )
    }
}
