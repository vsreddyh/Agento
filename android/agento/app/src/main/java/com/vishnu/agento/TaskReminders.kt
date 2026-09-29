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

/** One of the reminder points a task can fire at. */
internal enum class ReminderKind(val key: String, val lead: String) {
    /** `due - estimated_minutes`: the last moment you can start and still finish. */
    Start("start", "Start now"),

    /** [TaskReminders.HEADS_UP_MINUTES] before the due time. */
    Soon("soon", "Due in ${TaskReminders.HEADS_UP_MINUTES} min"),

    /** Due time has passed: repeats until the task is dealt with. */
    Overdue("overdue", "Overdue"),
}

/**
 * Fire times (epoch millis) for one **open** task that has a due time, or
 * an empty map when nothing is left to remind about.
 *
 * Pure and [nowMillis]-injected so the rules are readable in one place:
 * - a zero estimate gets no start nudge (there is nothing to start early
 *   for — the heads-up's job is to warn, not to schedule work);
 * - a start nudge and a heads-up landing on the same minute (estimate 5)
 *   arm once, not twice;
 * - points already in the past are dropped, so a task created late gets
 *   no burst of stale notifications;
 * - once the due time itself has passed the overdue nag is due
 *   immediately, and the receiver repeats it every
 *   [TaskReminders.OVERDUE_EVERY_MINUTES].
 */
internal fun reminderPoints(
    dueDate: String,
    dueTime: String,
    estimatedMinutes: Int,
    nowMillis: Long,
): Map<ReminderKind, Long> {
    val due = dueMillisOrNull(dueDate, dueTime) ?: return emptyMap()
    val out = LinkedHashMap<ReminderKind, Long>()
    if (estimatedMinutes > 0) {
        val start = due - estimatedMinutes * 60_000L
        if (start > nowMillis) out[ReminderKind.Start] = start
    }
    val soon = due - TaskReminders.HEADS_UP_MINUTES * 60_000L
    if (soon > nowMillis) out[ReminderKind.Soon] = soon
    if (due <= nowMillis) out[ReminderKind.Overdue] = due
    if (out[ReminderKind.Start] == out[ReminderKind.Soon]) out.remove(ReminderKind.Start)
    return out
}

/**
 * Due-time reminders for server tasks. Opt-in via the Task Manager bell
 * (default off: alarms must never surprise a fresh install). [refresh]
 * (re)computes the alarm set from **open** tasks that have a due time and
 * arms them; [TaskAlarmReceiver] posts the notification on fire,
 * deep-linking into the task's detail sheet. Called after every task
 * mutation, on boot, and when the Task Manager loads — never
 * periodically, so a quiet list costs nothing.
 *
 * A task gets up to three reminder points (see [reminderPoints]): a start
 * nudge at `due - estimated_minutes`, a heads-up 5 minutes before due, and
 * — once the due time has passed — a nag that repeats every
 * [OVERDUE_EVERY_MINUTES] minutes until the task is dealt with.
 *
 * Scope guardrails: only open tasks with a due date + time inside
 * [HORIZON_DAYS] are armed, capped at [MAX_ALARMS] nearest-first (three
 * per task, so the cap has to be generous to keep coverage); stale alarms
 * for completed/deleted/edited tasks are cancelled via the persisted key
 * set. Exact alarms are used when the OS grants them, inexact otherwise.
 */
object TaskReminders {

    private const val PREF_ARMED = "task_reminder_armed"
    private const val PREF_ENABLED = "task_reminders_enabled"
    const val CHANNEL_ID = "task_reminders"
    private const val HORIZON_DAYS = 7L
    private const val MAX_ALARMS = 60

    /** Heads-up lead time before the due time. */
    internal const val HEADS_UP_MINUTES = 5L

    /** How often an overdue task nags again while it stays unfinished. */
    internal const val OVERDUE_EVERY_MINUTES = 15L

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
            val nowMillis = LocalDateTime.now(IST).atZone(IST).toInstant().toEpochMilli()
            val horizon = LocalDate.now(IST).plusDays(HORIZON_DAYS).toString()
            val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
            // Nags the receiver already owns, carried through untouched.
            // Nearest fire time first so the cap keeps the urgent ones.
            val carried = HashSet<String>()
            val wanted = LinkedHashMap<String, Long>()
            for (t in tasks) {
                if (!t.dueDate.isIsoDate() || t.dueTime.isBlank()) continue
                if (t.dueDate > horizon) continue
                for ((kind, at) in reminderPoints(
                    t.dueDate, t.dueTime, t.estimatedMinutes, nowMillis,
                )) {
                    val key = alarmKey(t.id, kind)
                    // An armed nag owns its own cadence: re-arming it here
                    // would fire an extra alert on every refresh (which
                    // happens after each task mutation), and *dropping* it
                    // would cancel the nag before it ever repeated.
                    if (kind == ReminderKind.Overdue && key in armed) {
                        carried += key
                        continue
                    }
                    // The overdue point's own time is in the past by
                    // definition; nudge it a few seconds out so it fires
                    // once the refresh settles instead of never.
                    val fire = if (kind == ReminderKind.Overdue) {
                        nowMillis + 5_000L
                    } else {
                        at
                    }
                    wanted[key] = fire
                }
            }
            val capped = wanted.entries
                .sortedBy { (_, at) -> at }
                .take(MAX_ALARMS)
                .map { it.key }
                .toSet()
            // Carried nags sit outside the cap on purpose: they are already
            // committed, and cancelling a live nag to honour a cap is worse
            // than briefly exceeding it.
            val keep = LinkedHashSet(capped)
            keep.addAll(carried)
            for (key in armed) {
                if (key in keep) continue
                // Keys from 4.4 and earlier were a bare task id, armed with
                // a different PendingIntent identity (no "/<kind>" in the
                // data URI) — cancel it that way or it would fire once,
                // after the upgrade, for the old "due now" moment.
                if (key.contains('#')) cancelKind(appCtx, key) else cancelLegacy(appCtx, key)
            }
            for ((key, fire) in wanted) {
                if (key in capped) armAlarm(appCtx, key, fire)
            }
            prefs(appCtx).edit()
                .putStringSet(PREF_ARMED, keep)
                .apply()
        }
    }

    /** Drop every reminder for one task (all kinds). */
    fun cancelOne(context: Context, taskId: String) {
        val appCtx = context.applicationContext
        for (kind in ReminderKind.entries) cancelKind(appCtx, alarmKey(taskId, kind))
    }

    /**
     * The overdue nag, repeating. Owned by the receiver rather than
     * [refresh] so the 15-minute cadence is exact and a refresh can't
     * collapse it into a single alert. Re-armed only while the task is
     * still open and still past due — completing, deleting or rescheduling
     * it stops the nagging.
     */
    internal fun rearmOverdue(context: Context, taskId: String) {
        val appCtx = context.applicationContext
        val key = alarmKey(taskId, ReminderKind.Overdue)
        val at = System.currentTimeMillis() + OVERDUE_EVERY_MINUTES * 60_000L
        armAlarm(appCtx, key, at)
        val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
        if (key !in armed) {
            prefs(appCtx).edit().putStringSet(PREF_ARMED, armed + key).apply()
        }
    }

    /** Drop one armed alarm. The key is "<taskId>#<kind>". */
    private fun cancelKind(appCtx: Context, key: String) {
        val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        mgr.cancel(operation(appCtx, key))
        val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
        if (key in armed) {
            prefs(appCtx).edit().putStringSet(PREF_ARMED, armed - key).apply()
        }
    }

    /** Cancel a pre-4.5 alarm, whose identity had no kind segment. */
    private fun cancelLegacy(appCtx: Context, taskId: String) {
        val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        val fire = Intent(appCtx, TaskAlarmReceiver::class.java)
            .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
            .setData(Uri.parse("agento://reminder/$taskId"))
        mgr.cancel(
            PendingIntent.getBroadcast(
                appCtx, taskId.hashCode(), fire,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            ),
        )
        val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
        if (taskId in armed) {
            prefs(appCtx).edit().putStringSet(PREF_ARMED, armed - taskId).apply()
        }
    }

    /**
     * Set one alarm, exact when the OS grants it and inexact otherwise.
     * A revoked grant between the check and the call throws
     * SecurityException; falling back per alarm keeps one revocation from
     * aborting the loop.
     */
    private fun armAlarm(appCtx: Context, key: String, at: Long) {
        val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        val exact = Build.VERSION.SDK_INT < Build.VERSION_CODES.S ||
            mgr.canScheduleExactAlarms()
        val op = operation(appCtx, key)
        val armedOk = runCatching {
            if (exact) {
                mgr.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op)
            } else {
                mgr.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op)
            }
        }.isSuccess
        if (!armedOk && exact) {
            runCatching { mgr.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, op) }
        }
    }

    /** Alarm identity is one per task *and* kind: "<taskId>#<kind>". */
    private fun alarmKey(taskId: String, kind: ReminderKind) = "$taskId#${kind.key}"

    /** Drop every armed alarm (reminders disabled). Legacy bare-id keys
     * need the old identity, exactly as in refresh — otherwise a
     * pre-4.5 alarm survives the switch-off and fires afterwards. */
    fun cancelAll(context: Context) {
        val appCtx = context.applicationContext
        for (key in prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()) {
            if (key.contains('#')) cancelKind(appCtx, key) else cancelLegacy(appCtx, key)
        }
        prefs(appCtx).edit().remove(PREF_ARMED).apply()
    }

    private fun operation(appCtx: Context, key: String): PendingIntent {
        val (taskId, kind) = key.split('#', limit = 2)
            .let { it[0] to (it.getOrNull(1) ?: ReminderKind.Soon.key) }
        // Distinct data URI per key: the PendingIntent stays unique even
        // if two keys ever share a hashCode.
        val fire = Intent(appCtx, TaskAlarmReceiver::class.java)
            .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
            .putExtra(EXTRA_REMINDER_KIND, kind)
            .setData(Uri.parse("agento://reminder/$taskId/$kind"))
        return PendingIntent.getBroadcast(
            appCtx, key.hashCode(), fire,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
    }
}

/** Which reminder point fired; mirrors [ReminderKind.key]. */
internal const val EXTRA_REMINDER_KIND = "agento.reminder.kind"

private fun reminderKindOrNull(raw: String?): ReminderKind? =
    ReminderKind.entries.firstOrNull { it.key == raw }

/** Fires a due task's notification; tap deep-links to its detail sheet.
 * Name lookup runs off-thread via goAsync so a slow network can never
 * ANR the broadcast — the alert still posts on fallback title if lookup
 * fails. The scope is file-shared (like [TaskReminders]'s), not per
 * broadcast, so fired receivers leave nothing behind. */
class TaskAlarmReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val taskId = intent.getStringExtra(TaskWidget.EXTRA_TASK_ID).orEmpty()
        if (taskId.isEmpty()) return
        // Pre-4.x alarms (and any that lost their extra) land as Soon: one
        // heads-up is the old "due now" behaviour, not a nag loop.
        val kind = reminderKindOrNull(intent.getStringExtra(EXTRA_REMINDER_KIND))
            ?: ReminderKind.Soon
        val pending = goAsync()
        alarmScope.launch {
            try {
                post(context.applicationContext, taskId, kind)
            } finally {
                pending.finish()
            }
        }
    }

    private suspend fun post(appCtx: Context, taskId: String, kind: ReminderKind) {
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
        // The overdue nag is the one self-repeating point: keep it going
        // only while the task is still open and still past due, so
        // completing, deleting or rescheduling it stops the nagging. The
        // one-shot points are the receiver's job to forget — their time
        // has passed either way.
        if (kind == ReminderKind.Overdue) {
            val due = task?.let { dueMillisOrNull(it.dueDate, it.dueTime) }
            if (task != null && due != null && due <= System.currentTimeMillis()) {
                TaskReminders.rearmOverdue(appCtx, taskId)
            } else if (open != null) {
                // The fetch worked and the task is no longer past due: it
                // was rescheduled, so stop the nag and say nothing.
                TaskReminders.cancelOne(appCtx, taskId)
                return
            } else {
                // Fetch failed: unknowable, so keep the cadence rather than
                // guessing. This alarm is already consumed, and refresh
                // carries an armed key instead of re-creating it — so
                // staying quiet here would end the nag until some unrelated
                // edit happened to re-derive it. The next successful fetch
                // drops it the moment the task is gone or rescheduled.
                TaskReminders.rearmOverdue(appCtx, taskId)
            }
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
        // The estimate is an input, not something to show: what the user
        // needs is when to start and when it is due.
        val startLine = task?.startParts()?.let { (d, t) -> friendlyDue(d, t, today) }
            ?.takeIf { it.isNotEmpty() }
        val summary = listOf(
            kind.lead,
            dueLine.ifEmpty { null },
            startLine?.let { "start $it" },
        ).filterNotNull().joinToString(" · ")
        // Locals: task is nullable and conditions below don't smart-cast.
        val taskDesc = task?.description.orEmpty()
        val repeat = task?.repeatLabel().orEmpty()
        // Lead line says which reminder this is, so a start nudge and a
        // heads-up never read as the same alert.
        val lead = when (kind) {
            ReminderKind.Start ->
                "Start now${dueLine.ifEmpty { "" }.let { if (it.isEmpty()) "" else ", due $it" }}."
            ReminderKind.Soon ->
                "Due in ${TaskReminders.HEADS_UP_MINUTES} minutes."
            ReminderKind.Overdue ->
                "Overdue${if (dueLine.isNotEmpty()) " since $dueLine" else ""}."
        }
        val big = buildList {
            add(lead)
            if (taskDesc.isNotEmpty()) add(taskDesc)
            if (dueLine.isNotEmpty()) add(dueLine)
            if (startLine != null) add("Starts $startLine")
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
            // Tag-based identity: unique per task *and* reminder point, so
            // a task's start nudge, heads-up and each overdue nag occupy
            // their own slot instead of replacing one another. Sharing the
            // old taskId-only tag meant a task's later reminder silently
            // overwrote the earlier one still waiting in the shade.
            // (Two ids sharing a hashCode can't collide either — the
            // PendingIntent request code is disambiguated by its data URI.)
            "$taskId#${kind.key}", ALARM_NOTIF_ID,
            notif.build(),
        )
    }
}
