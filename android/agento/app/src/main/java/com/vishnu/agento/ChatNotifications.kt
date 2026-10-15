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
import androidx.core.content.edit
import androidx.core.net.toUri

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
            .edit {
                putBoolean(KEY_NOTIFY_DONE, enabled)
            }
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
    fun notifyDone(context: Context, tabTitle: String, snippet: String) {        if (!isEnabled(context) || !canPost(context)) return
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
     * Posts a reply-failed notification (#287): the turn was cut off
     * (dropped stream) or errored while backgrounded, and the partial text
     * (if any) is a fragment, not an answer. Same per-tab slot as
     * [notifyDone] — one tab owns one card, whatever the outcome — so a
     * crash replaces the pending ping rather than stacking beside it.
     */
    fun notifyCrashed(context: Context, tabTitle: String, snippet: String) {
        if (!isEnabled(context) || !canPost(context)) return
        ensureChannel(context)
        val open = PendingIntent.getActivity(
            context, tabTitle.hashCode(),
            Intent(context, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val text = snippet.ifBlank { "The reply was cut off — tap to continue." }
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_notify_error)
            .setContentTitle("$tabTitle reply failed")
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setContentIntent(open)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_DEFAULT)
            .build()
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.notify(NOTIFICATION_ID + tabTitle.hashCode(), notification)
    }

    /**
     * Posts the task-instruction saved card (#287): tap opens the new chat
     * thread and auto-sends it exactly once. One slot per thread, so two
     * instructions never overwrite each other.
     */
    fun notifyInstructSaved(context: Context, taskName: String, threadId: String) {
        if (!isEnabled(context) || !canPost(context)) return
        ensureChannel(context)
        val open = PendingIntent.getActivity(
            context, ("instruct-open:$threadId").hashCode(),
            Intent(context, MainActivity::class.java)
                .setAction(InstructIntents.ACTION_CHAT)
                .putExtra(InstructIntents.EXTRA_THREAD_ID, threadId)
                .putExtra(InstructIntents.EXTRA_AUTOSEND, true)
                .setData("agento://chat/${android.net.Uri.encode(threadId)}".toUri()),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val text = "Instruction for \"$taskName\" saved — tap to start the chat."
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle("Instruction saved")
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setContentIntent(open)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_DEFAULT)
            .build()
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.notify(NOTIFICATION_ID + ("instruct:$threadId").hashCode(), notification)
    }

    /**
     * Posts a cron-response notification on the same replies channel; tap
     * opens the stored cron chat. Returns whether anything posted, so the
     * caller only advances its seen cursor past runs actually delivered.
     */
    fun notifyCron(
        context: Context,
        jobId: String,
        jobName: String,
        snippet: String,
        moreCount: Int = 0,
    ): Boolean {
        if (!isEnabled(context) || !canPost(context)) return false
        ensureChannel(context)
        val title = jobName.trim().ifEmpty { "Scheduled job" }
        val text = if (moreCount > 0) "$snippet (+$moreCount more)" else snippet
        val open = PendingIntent.getActivity(
            context, ("cron:$jobId").hashCode(),
            Intent(context, MainActivity::class.java)
                .setAction(CronIntents.ACTION_CRON)
                .putExtra(CronIntents.EXTRA_JOB_ID, jobId)
                .setData("agento://cron/${android.net.Uri.encode(jobId)}".toUri()),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setContentIntent(open)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_DEFAULT)
            .build()
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        // One slot per job: successive responses on a job replace the
        // previous, so the shade never stacks one card per fire.
        nm.notify(NOTIFICATION_ID + ("cron:$jobId").hashCode(), notification)
        return true
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
