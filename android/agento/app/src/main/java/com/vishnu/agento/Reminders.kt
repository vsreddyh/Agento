package com.vishnu.agento

import android.app.AlarmManager
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Build
import androidx.core.app.NotificationCompat

/** Local reminders: exact alarms + notification channel (#31, #32). */
object Reminders {

    const val CHANNEL_ID = "reminders"
    const val ACTION_FIRE = "com.vishnu.agento.FIRE_REMINDER"
    const val EXTRA_ID = "reminder_id"
    const val EXTRA_TITLE = "reminder_title"
    const val EXTRA_TEXT = "reminder_text"

    /** Stable non-negative request code from a UUID string (hashCode can
     * collide AND go negative — both break alarm identity). */
    fun requestCode(id: String): Int =
        (id.hashCode().toLong() and 0x7fffffffL).toInt()

    fun ensureChannel(context: Context) {
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (nm.getNotificationChannel(CHANNEL_ID) == null) {
            nm.createNotificationChannel(
                NotificationChannel(
                    CHANNEL_ID,
                    "Reminders",
                    NotificationManager.IMPORTANCE_HIGH,
                ).apply { description = "Scheduled reminders and timers" },
            )
        }
    }

    /** True when exact alarms are allowed (always true below Android 12). */
    fun canScheduleExact(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S) return true
        val am = context.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        return am.canScheduleExactAlarms()
    }

    fun schedule(context: Context, item: ReminderItem) {
        ensureChannel(context)
        val am = context.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        val intent = Intent(context, ReminderReceiver::class.java).apply {
            action = ACTION_FIRE
            putExtra(EXTRA_ID, item.id)
            putExtra(EXTRA_TITLE, item.title)
            putExtra(EXTRA_TEXT, item.text)
        }
        val pi = PendingIntent.getBroadcast(
            context, requestCode(item.id), intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        if (canScheduleExact(context)) {
            am.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, item.atEpoch, pi)
        } else {
            // Inexact fallback; the permission screen explains the tradeoff.
            am.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, item.atEpoch, pi)
        }
    }

    fun cancel(context: Context, id: String) {
        val am = context.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        val intent = Intent(context, ReminderReceiver::class.java).apply {
            action = ACTION_FIRE
        }
        val pi = PendingIntent.getBroadcast(
            context, requestCode(id), intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        am.cancel(pi)
    }

    /** Re-arms all future reminders (boot, update, or permission grant). */
    fun rescheduleAll(context: Context) {
        val now = System.currentTimeMillis()
        ReminderStore.load(context)
            .filter { it.atEpoch > now }
            .forEach { schedule(context, it) }
    }
}

/** Fires a scheduled reminder as a notification; removes it from the store. */
class ReminderReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Reminders.ACTION_FIRE) return
        val id = intent.getStringExtra(Reminders.EXTRA_ID).orEmpty()
        val title = intent.getStringExtra(Reminders.EXTRA_TITLE).orEmpty()
        val text = intent.getStringExtra(Reminders.EXTRA_TEXT).orEmpty()
        Reminders.ensureChannel(context)

        val open = PendingIntent.getActivity(
            context, 0,
            Intent(context, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(context, Reminders.CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_dialog_info)
            .setContentTitle(title.ifEmpty { "Reminder" })
            .setContentText(text.ifEmpty { "Time's up." })
            .setContentIntent(open)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .build()
        val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.notify(requestCode(id.ifEmpty { title + text }), notification)

        if (id.isNotEmpty()) {
            // Store IO must not run on the broadcast main thread.
            Thread {
                val remaining = ReminderStore.load(context).filterNot { it.id == id }
                ReminderStore.save(context, remaining)
            }.start()
        }
    }
}
