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
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import java.time.LocalDate
import java.time.LocalDateTime

private val IsoDate = Regex("\\d{4}-\\d{2}-\\d{2}")
private val ClockTime = Regex("(\\d{1,2}):(\\d{2})(?::(\\d{2}))?")

/** Shared worker for alarm receivers (one scope, never per-broadcast). */
private val alarmScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

/** Fixed notification id; identity comes from the per-task tag. */
internal const val ALARM_NOTIF_ID = 1

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
    // The :ss group is optional — absent means zero, never "".toInt().
    val hh = h.toIntOrNull() ?: return null
    val mm = m.toIntOrNull() ?: return null
    val ss = s.toIntOrNull() ?: 0
    if (hh > 23 || mm > 59 || ss > 59) return null
    val at = runCatching {
        LocalDate.parse(dueDate).atTime(hh, mm, ss)
    }.getOrNull() ?: return null
    return at.atZone(IST).toInstant().toEpochMilli()
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

    /** Recompute alarms off-thread. Safe to call often; diffs only.
     * Returns the worker Job so callers that must outlive a broadcast
     * (BootReceiver) can join it. */
    fun refresh(context: Context): Job {
        val appCtx = context.applicationContext
        if (!isEnabled(appCtx)) return Job().also { it.complete() }
        return scope.launch {
            val tasks = TasksApi(appCtx).list("open").getOrNull() ?: return@launch
            // IST-pinned (#124): due times are entered as wall-clock IST,
            // so "now" and the arming horizon must read the same zone.
            val now = LocalDateTime.now(IST)
            val horizon = LocalDate.now(IST).plusDays(HORIZON_DAYS).toString()
            // Nearest fire time first so the cap keeps the urgent ones.
            val wanted = tasks
                .filter { it.dueDate.isIsoDate() && it.dueTime.isNotBlank() }
                .filter { it.dueDate <= horizon }
                .mapNotNull { t ->
                    val at = dueMillisOrNull(t.dueDate, t.dueTime) ?: return@mapNotNull null
                    if (at <= now.atZone(IST).toInstant().toEpochMilli()) {
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
                // A revoked grant between the check and the call throws
                // SecurityException; fall back to inexact per alarm so one
                // revocation can't abort the loop or skip the armed-write.
                val armedOk = runCatching {
                    if (exact) {
                        mgr.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op)
                    } else {
                        mgr.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op)
                    }
                }.isSuccess
                if (!armedOk && exact) {
                    runCatching {
                        mgr.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op)
                    }
                }
            }
            prefs(appCtx).edit()
                .putStringSet(PREF_ARMED, wanted.keys.toSet())
                .apply()
        }
    }

    /** Drop one armed alarm (fired for a task that's gone). */
    fun cancelOne(context: Context, taskId: String) {
        val appCtx = context.applicationContext
        val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        mgr.cancel(operation(appCtx, taskId))
        val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
        if (taskId in armed) {
            prefs(appCtx).edit().putStringSet(PREF_ARMED, armed - taskId).apply()
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
 * ANR the broadcast — the alert still posts on fallback title if lookup
 * fails. The scope is file-shared (like [TaskReminders]'s), not per
 * broadcast, so fired receivers leave nothing behind. */
class TaskAlarmReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val taskId = intent.getStringExtra(TaskWidget.EXTRA_TASK_ID).orEmpty()
        if (taskId.isEmpty()) return
        val pending = goAsync()
        alarmScope.launch {
            try {
                post(context.applicationContext, taskId)
            } finally {
                pending.finish()
            }
        }
    }

    private suspend fun post(appCtx: Context, taskId: String) {
        val open = withTimeoutOrNull(10_000) {
            TasksApi(appCtx).list("open").getOrNull()
        }
        // Absent from a successful fetch means done/deleted after arming
        // (e.g. agent-side while the app was closed): drop the alarm and
        // stay silent instead of pinging for a finished task. A failed
        // fetch (null) is unknowable — the alert still goes out generic.
        val task = open?.firstOrNull { it.id == taskId }
        if (open != null && task == null) {
            TaskReminders.cancelOne(appCtx, taskId)
            return
        }
        val name = task?.name.orEmpty().ifEmpty { "Task due" }
        // The fetch already carries the whole task — surface it instead
        // of a bare "Due now" (#130 follow-up).
        val today = LocalDate.now(IST)
        val dueLine = if (task != null) {
            friendlyDue(task.dueDate, task.dueTime, today)
        } else {
            ""
        }
        val summary = listOf(
            dueLine.ifEmpty { null },
            task?.estimatedMinutes?.takeIf { it > 0 }?.let { "~$it min" },
        ).filterNotNull().joinToString(" · ").ifEmpty { "Due now" }
        // Locals: task is nullable and conditions below don't smart-cast.
        val taskDesc = task?.description.orEmpty()
        val mins = task?.estimatedMinutes ?: 0
        val repeat = task?.repeatRule.orEmpty()
        val big = buildList {
            if (taskDesc.isNotEmpty()) add(taskDesc)
            if (dueLine.isNotEmpty()) add(dueLine)
            if (mins > 0) add("Estimate ~$mins min")
            if (repeat.isNotEmpty()) add("Repeats $repeat")
            if (task?.parallelable == true) add("Can run in parallel")
        }.joinToString("\n")
        // Inline complete reuses the widget trampoline (no visible UI:
        // completes, refreshes widget/alarms, finishes). Data URI + own
        // request code: extras don't count for PendingIntent identity,
        // so without these one task's Done could complete another.
        val done = PendingIntent.getActivity(
            appCtx, ("done:$taskId").hashCode(),
            Intent(appCtx, TaskCompleteActivity::class.java)
                .putExtra(TaskWidget.EXTRA_COMPLETE_ID, taskId)
                .setData(Uri.parse("agento://reminder/$taskId/complete")),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val mgr = appCtx.getSystemService(Context.NOTIFICATION_SERVICE)
            as NotificationManager
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            mgr.createNotificationChannel(
                NotificationChannel(
                    TaskReminders.CHANNEL_ID,
                    appCtx.getString(R.string.task_reminder_channel),
                    NotificationManager.IMPORTANCE_HIGH,
                ).apply {
                    this.description = appCtx.getString(R.string.task_reminder_channel_desc)
                },
            )
        }
        val launch = Intent(appCtx, MainActivity::class.java)
            .setAction(TaskWidget.ACTION_TASKS)
            .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
            .setData(Uri.parse("agento://reminder/$taskId"))
        val tap = PendingIntent.getActivity(
            appCtx, taskId.hashCode(), launch,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notif = NotificationCompat.Builder(appCtx, TaskReminders.CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_menu_agenda)
            .setContentTitle(name)
            .setContentText(summary)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setAutoCancel(true)
            .setContentIntent(tap)
            // No icon (0): framework checkables render badly as action
            // icons on some OEMs.
            .addAction(0, "Done", done)
        // Expanded view only when there's a description —
        // otherwise it duplicates the summary in a second page.
        if (taskDesc.isNotEmpty()) {
            notif.setStyle(
                NotificationCompat.BigTextStyle().bigText(big),
            )
        }
        mgr.notify(
            // Tag-based identity: unique per task even if two ids ever
            // share a hashCode (the PendingIntent request code, by
            // contrast, is disambiguated by its data URI).
            taskId, ALARM_NOTIF_ID,
            notif.build(),
        )
    }
}
