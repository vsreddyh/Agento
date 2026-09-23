package com.vishnu.agento

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat

/**
 * Hermes reply notifications (issue #58).
 *
 * The gateway config has no scheduler/cron section, so there are no
 * server-side cronjobs whose completion could push anything. What the app
 * CAN do client-side is notify when a chat reply finishes while the user
 * isn't looking at the app (backgrounded). That toggle lives in
 * Settings → Hermes notifications as `notify_reply_done`.
 */
object ChatNotifications {

    const val CHANNEL_ID = "hermes_replies"
    const val KEY_NOTIFY_DONE = "notify_reply_done"
    private const val NOTIFICATION_ID = 4252

    fun isEnabled(context: Context): Boolean =
        context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
            .getBoolean(KEY_NOTIFY_DONE, true)

    fun setEnabled(context: Context, enabled: Boolean) {
        context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
            .edit().putBoolean(KEY_NOTIFY_DONE, enabled).apply()
    }

    /** True when posting is allowed (pre-33 always; 33+ needs the runtime grant). */
    fun canPost(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return true
        return ContextCompat.checkSelfPermission(
            context, android.Manifest.permission.POST_NOTIFICATIONS
        ) == PackageManager.PERMISSION_GRANTED
    }

    fun ensureChannel(context: Context) {
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (nm.getNotificationChannel(CHANNEL_ID) == null) {
            nm.createNotificationChannel(
                NotificationChannel(
                    CHANNEL_ID,
                    "Hermes replies",
                    NotificationManager.IMPORTANCE_DEFAULT,
                ).apply { description = "Posted when a Hermes reply finishes while the app is in the background" },
            )
        }
    }

    /** Posts a reply-finished notification; no-ops when disabled or not permitted. */
    fun notifyDone(context: Context, tabTitle: String, snippet: String) {
        if (!isEnabled(context) || !canPost(context)) return
        ensureChannel(context)
        val open = PendingIntent.getActivity(
            context, tabTitle.hashCode(),
            Intent(context, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle("$tabTitle replied")
            .setContentText(snippet.ifBlank { "Your Hermes reply is ready." })
            .setStyle(
                NotificationCompat.BigTextStyle()
                    .bigText(snippet.ifBlank { "Your Hermes reply is ready." })
            )
            .setContentIntent(open)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_DEFAULT)
            .build()
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        // Stable per-tab ID (no 0xFF bucketing) so tabs never overwrite
        // each other; successive replies on one tab replace the previous.
        nm.notify(NOTIFICATION_ID + tabTitle.hashCode(), notification)
    }

    /**
     * Manual fire button for Settings → Hermes notifications.
     * Returns false (nothing posted) when the toggle is off or the
     * runtime grant is denied, so the caller can say that instead of
     * claiming success.
     */
    fun sendTest(context: Context): Boolean {
        if (!isEnabled(context) || !canPost(context)) return false
        ensureChannel(context)
        notifyDone(context, "Agento", "Test notification — Hermes reply alerts are working.")
        return true
    }
}
