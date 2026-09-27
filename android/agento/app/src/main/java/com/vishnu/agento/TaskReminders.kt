package com.vishnu.agento

import android.app.AlarmManager
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import androidx.core.app.NotificationCompat
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.ZoneId

private val IsoDate = Regex("\\d{4}-\\d{2}-\\d{2}")
private val ClockTime = Regex("(\\d{1,2}):(\\d{2})(?::(\\d{2}))?")

/** True for strict ISO dates only; anything else is treated as undated
 * so one malformed row can never poison comparisons or alarms. */
fun String.isIsoDate(): Boolean =
    matches(IsoDate) && runCatching { LocalDate.parse(this) }.isSuccess

/** Due datetime as epoch millis, or null. Accepts H:mm / HH:mm with an
 * optional :ss; anything else (blank, "9am", tz suffixes) is skipped
 * rather than silently dropped by a failed parse. */
fun dueMillisOrNull(dueDate: String, dueTime: String): Long? {
    if (!dueDate.isIsoDate()) return null
    val parts = ClockTime.matchEntire(dueTime.trim()) ?: return null
    val (h, m, s) = parts.destructured
    if (h.toInt() > 23 || m.toInt() > 59 || s.toInt() > 59) return null
    val at = runCatching {
        LocalDate.parse(dueDate).atTime(h.toInt(), m.toInt(), s.toInt())
    }.getOrNull() ?: return null
    return at.atZone(ZoneId.systemDefault()).toInstant().toEpochMilli()
}

/**
 * Due-time reminders for server tasks. Opt-in via the Task Manager bell
 * (default off: alarms must never surprise a fresh install). [refresh]
 * (re)computes the alarm set from open tasks with a due date + time and
 * arms the nearest ones; [TaskAlarmReceiver] posts the notification on
 * fire, deep-linking into the task's detail sheet. Called after every
 * task mutation, on boot, and when the Task Manager loads — never
 * periodically, so a quiet list costs nothing.
 *
 * Scope guardrails: only tasks due within [HORIZON_DAYS] with a time
 * component are armed, capped at [MAX_ALARMS] nearest-first; stale
 * alarms for completed/deleted/edited tasks are cancelled via the
 * persisted id set. Exact alarms are used when the OS grants them,
 * inexact otherwise.
 */
object TaskReminders {

    private const val PREF_ARMED = "task_reminder_armed"
    private const val PREF_ENABLED = "task_reminders_enabled"
    const val CHANNEL_ID = "task_reminders"
    private const val HORIZON_DAYS = 7L
    private const val MAX_ALARMS = 25

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    fun isEnabled(context: Context): Boolean =
        prefs(context).getBoolean(PREF_ENABLED, false)

    fun setEnabled(context: Context, enabled: Boolean) {
        prefs(context).edit().putBoolean(PREF_ENABLED, enabled).apply()
        if (enabled) refresh(context) else cancelAll(context)
    }

    private fun prefs(context: Context) =
        context.applicationContext.getSharedPreferences(
            AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)

    /** Recompute alarms off-thread. Safe to call often; diffs only. */
    fun refresh(context: Context) {
        val appCtx = context.applicationContext
        if (!isEnabled(appCtx)) return
        scope.launch {
            val tasks = TasksApi(appCtx).list("open").getOrNull() ?: return@launch
            val now = LocalDateTime.now()
            val horizon = LocalDate.now().plusDays(HORIZON_DAYS).toString()
            // Nearest fire time first so the cap keeps the urgent ones.
            val wanted = tasks
                .filter { it.dueDate.isIsoDate() && it.dueTime.isNotBlank() }
                .filter { it.dueDate <= horizon }
                .mapNotNull { t ->
                    val at = dueMillisOrNull(t.dueDate, t.dueTime) ?: return@mapNotNull null
                    if (at <= now.atZone(ZoneId.systemDefault()).toInstant().toEpochMilli()) {
                        null
                    } else {
                        t.id to at
                    }
                }
                .sortedBy { (_, at) -> at }
                .take(MAX_ALARMS)
                .toMap()
            val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
            val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
            // Cancel anything no longer wanted (done, deleted, edited, past).
            for (id in armed) {
                if (id !in wanted) mgr.cancel(operation(appCtx, id))
            }
            val exact = Build.VERSION.SDK_INT < Build.VERSION_CODES.S ||
                mgr.canScheduleExactAlarms()
            for ((id, at) in wanted) {
                val op = operation(appCtx, id)
                if (exact) {
                    mgr.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op)
                } else {
                    mgr.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op)
                }
            }
            prefs(appCtx).edit()
                .putStringSet(PREF_ARMED, wanted.keys.toSet())
                .apply()
        }
    }

    /** Drop every armed alarm (reminders disabled). */
    fun cancelAll(context: Context) {
        val appCtx = context.applicationContext
        val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        for (id in prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()) {
            mgr.cancel(operation(appCtx, id))
        }
        prefs(appCtx).edit().remove(PREF_ARMED).apply()
    }

    private fun operation(appCtx: Context, taskId: String): PendingIntent {
        // Distinct data URI per task: the PendingIntent stays unique
        // even if two ids ever share a hashCode.
        val fire = Intent(appCtx, TaskAlarmReceiver::class.java)
            .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
            .setData(Uri.parse("agento://reminder/$taskId"))
        return PendingIntent.getBroadcast(
            appCtx, taskId.hashCode(), fire,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
    }
}

/** Fires a due task's notification; tap deep-links to its detail sheet.
 * Name lookup runs off-thread via goAsync so a slow network can never
 * ANR the broadcast — the alert posts with a fallback title first only
 * if the lookup path itself is slow, otherwise with the real name. */
class TaskAlarmReceiver : BroadcastReceiver() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onReceive(context: Context, intent: Intent) {
        val taskId = intent.getStringExtra(TaskWidget.EXTRA_TASK_ID).orEmpty()
        if (taskId.isEmpty()) return
        val pending = goAsync()
        scope.launch {
            try {
                post(context.applicationContext, taskId)
            } finally {
                pending.finish()
            }
        }
    }

    private suspend fun post(appCtx: Context, taskId: String) {
        val name = withTimeoutOrNull(10_000) {
            TasksApi(appCtx).list("open").getOrNull()
        }?.firstOrNull { it.id == taskId }?.name
            .orEmpty().ifEmpty { "Task due" }
        val mgr = appCtx.getSystemService(Context.NOTIFICATION_SERVICE)
            as NotificationManager
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            mgr.createNotificationChannel(
                NotificationChannel(
                    TaskReminders.CHANNEL_ID,
                    appCtx.getString(R.string.task_reminder_channel),
                    NotificationManager.IMPORTANCE_HIGH,
                ).apply {
                    description = appCtx.getString(R.string.task_reminder_channel_desc)
                },
            )
        }
        val open = Intent(appCtx, MainActivity::class.java)
            .setAction(TaskWidget.ACTION_TASKS)
            .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
            .setData(Uri.parse("agento://reminder/$taskId"))
        val tap = PendingIntent.getActivity(
            appCtx, taskId.hashCode(), open,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        mgr.notify(
            taskId.hashCode(),
            NotificationCompat.Builder(appCtx, TaskReminders.CHANNEL_ID)
                .setSmallIcon(android.R.drawable.ic_menu_agenda)
                .setContentTitle(name)
                .setContentText("Due now")
                .setPriority(NotificationCompat.PRIORITY_HIGH)
                .setAutoCancel(true)
                .setContentIntent(tap)
                .build(),
        )
    }
}
