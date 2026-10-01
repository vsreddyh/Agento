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
import kotlinx.coroutines.Deferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
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
/** The four reminders a task gets (issue #160), in the order they happen.
 *
 * A reminder is a *moment*, not a field: the copy for each is written
 * against what has just happened, so the lock screen says one useful thing
 * rather than echoing the task back. [reminderMessage] holds the wording. */
internal enum class ReminderKind(val key: String) {
    /** [TaskReminders.BEFORE_START_MINUTES] before the start time. */
    BeforeStart("before"),

    /** `due - estimated_minutes`: the last moment you can start and still finish. */
    Start("start"),

    /** The due time itself. */
    Due("due"),

    /** Due time has passed: repeats until the task is dealt with. */
    Overdue("overdue"),
}

/**
 * Fire times (epoch millis) for one **open** task that has a due time, or
 * an empty map when nothing is left to remind about.
 *
 * Pure and [nowMillis]-injected so the rules are readable in one place:
 * - points already in the past are dropped, so a task created late gets no
 *   burst of stale notifications;
 * - points closer together than [TaskReminders.MIN_GAP_MINUTES] collapse
 *   into the later one, so a tiny estimate cannot produce "Start now" and
 *   "Due now" a minute apart;
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
    if (due <= nowMillis) {
        out[ReminderKind.Overdue] = due
        return out
    }
    // The start rule itself is not re-derived here: [startMomentMillis] is
    // the same definition the list buckets and sorts with, so "starts now"
    // cannot mean one thing in the reminder engine and another in the row
    // the user is looking at (#164).
    val start = startMomentMillis(due, estimatedMinutes)
    if (start != null) {
        // A zero estimate has no start of its own, so there is no start time
        // for the 5-minute warning to lead into and "Start now" would fire
        // *at* the due moment saying the wrong thing: such a task gets the
        // due alert alone.
        addIfFuture(
            out,
            ReminderKind.BeforeStart,
            start - TaskReminders.BEFORE_START_MINUTES * 60_000L,
            nowMillis,
        )
        addIfFuture(out, ReminderKind.Start, start, nowMillis)
    }
    addIfFuture(out, ReminderKind.Due, due, nowMillis)
    // Collapse reminders that would land on top of each other: a one-minute
    // estimate would otherwise fire "Start now" and "Due now" a minute
    // apart, which is one alert too many for one moment. Exact-equality was
    // not enough here — the points are a minute quantum apart, never
    // identical.
    //
    // The *later* reminder wins, because the deadline is the alert that must
    // not be missed: dropping "Start now" and keeping "Due now" loses
    // nothing, where the reverse leaves a task that has just come due
    // silent.
    val gap = TaskReminders.MIN_GAP_MINUTES * 60_000L
    // Nullable, not a Long sentinel: `at - Long.MIN_VALUE` overflows and
    // comes back negative, which would drop the first reminder of every
    // task — and for a task whose only point is Due, all of it.
    var lastKind: ReminderKind? = null
    var lastAt: Long? = null
    for (kind in ReminderKind.entries) {
        val at = out[kind] ?: continue
        val prevAt = lastAt
        if (lastKind != null && prevAt != null && at - prevAt < gap) {
            out.remove(lastKind)
        }
        // Always advance, including after a collapse: `last` has to be the
        // point that won, or the next comparison is against a time that is
        // no longer in the map. With today's constants only one collapse
        // can fire, so this is latent rather than live.
        lastKind = kind
        lastAt = at
    }
    return out
}

/**
 * The one line each reminder says (issue #160).
 *
 * A lock-screen line has one job: say what just happened, and the one time
 * that matters. The four types are written to be distinguishable at a
 * glance in a notification stack, because a "Start now" and a "Due now"
 * that read the same are worse than no reminder at all.
 *
 * [dueLine] and [startLine] are already formatted ("Today, 09:00"). When
 * the task could not be fetched, [dueLine] is "" and [startLine] is null,
 * and the wording degrades to the bare fact rather than inventing a time.
 */
private fun reminderMessage(
    kind: ReminderKind,
    dueLine: String,
    startLine: String?,
): String = when (kind) {
    // The actionable time differs per reminder, which is why each line is
    // written separately rather than composed from one suffix: before the
    // start, the start time is what you act on; once started, the deadline
    // is what you are racing; at the deadline, there is nothing left to add.
    ReminderKind.BeforeStart ->
        "Starting in ${TaskReminders.BEFORE_START_MINUTES} minutes" +
            suffix(startLine, "starts")
    ReminderKind.Start ->
        "Start now" + suffix(dueLine, "due")
    // No suffix on purpose: at the due moment the time *is* now, and "Due
    // now · due 09:00" says the same thing twice. The expanded view below
    // it still carries the window.
    ReminderKind.Due ->
        "Due now"
    ReminderKind.Overdue ->
        if (dueLine.isEmpty()) "Overdue" else "Overdue since $dueLine"
}

/** " · due Today, 09:00", or nothing when there is no time to show. */
private fun suffix(line: String?, label: String): String =
    if (line.isNullOrEmpty()) "" else " \u00b7 $label $line"

private fun addIfFuture(
    out: MutableMap<ReminderKind, Long>,
    kind: ReminderKind,
    at: Long,
    nowMillis: Long,
) {
    if (at > nowMillis) out[kind] = at
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
 * A task gets up to three armed reminder points (see [reminderPoints]) —
 * five minutes before the start, at the start, and at the due time — plus,
 * once the due time has passed, a nag that repeats every
 * [OVERDUE_EVERY_MINUTES] minutes until the task is dealt with.
 *
 * Scope guardrails: only open tasks with a due date + time inside
 * [HORIZON_DAYS] are armed, capped at [MAX_ALARMS] with one-shot alerts
 * ranked ahead of repeating nags so the budget never silently eats the
 * alert that is about to fire; stale alarms for completed/deleted/edited
 * tasks are cancelled via the persisted key set. Exact alarms are used when
 * the OS grants them, inexact otherwise.
 *
 * What the cap had to leave out is recorded in [Budget] and reported by
 * the diagnostics card — over the cap is a condition the user can see,
 * not one they infer from a reminder that never came (#165).
 */
object TaskReminders {

    private const val PREF_ARMED = "task_reminder_armed"
    private const val PREF_BUDGET = "task_reminder_budget"
    private const val PREF_ENABLED = "task_reminders_enabled"
    const val CHANNEL_ID = "task_reminders"
    private const val HORIZON_DAYS = 7L
    // An overdue point's own time is in the past by definition, so every nag
    // is nudged a few seconds out to fire once a refresh settles. Carried and
    // new alike share this offset, so the nag group is ordered purely by how
    // long a task has been overdue — with no accidental head start for
    // whichever nag happens to be armed already.
    private const val NAG_FIRE_OFFSET = 5_000L
    // A self-imposed guard, not a platform limit: Android allows far more,
    // and Samsung's documented ceiling is 500 pending alarms per app. At
    // three armed points per task, 200 covers roughly 65 timed tasks — past
    // the point where the list itself needs paging (#168). What the cap
    // protects is the OS's ability to hold them, so it is a floor for
    // correctness, not a ceiling for capacity (#165).
    private const val MAX_ALARMS = 200

    /** Lead time before the start time, for [ReminderKind.BeforeStart]. */
    internal const val BEFORE_START_MINUTES = 5L

    /**
     * Two reminders closer together than this are one reminder: a shorter
     * gap than the user can act on produces a burst, not a warning. A
     * one-minute estimate is the case this exists for.
     */
    internal const val MIN_GAP_MINUTES = 2L

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

    /**
     * Recompute alarms off-thread. Safe to call often; diffs only.
     *
     * Returns a [Deferred] rather than a plain Job, so the outcome travels
     * back as a value the caller can await: a Job that completes is a Job
     * that threw nothing, which is no way to tell [TaskSyncWorker] that its
     * half-hourly run fetched nothing. Reminders switched off counts as
     * [RefreshOutcome.Ok] — there was nothing to do, and nothing failed.
     */
    fun refresh(context: Context): Deferred<RefreshOutcome> {
        val appCtx = context.applicationContext
        return scope.async {
            if (!isEnabled(appCtx)) return@async RefreshOutcome.Ok
            val tasks = TasksApi(appCtx).list("open").getOrNull()
                ?: return@async RefreshOutcome.Failed
            // IST-pinned (#124): due times are entered as wall-clock IST,
            // so "now" and the arming horizon must read the same zone.
            val nowMillis = LocalDateTime.now(IST).atZone(IST).toInstant().toEpochMilli()
            val horizon = LocalDate.now(IST).plusDays(HORIZON_DAYS).toString()
            // One critical section from here to the final write. Everything
            // below is CPU-only plus the arm/cancel calls, and the point is
            // that an alarm firing on another thread cannot edit PREF_ARMED
            // between this read and that write. The network fetch above is
            // deliberately outside it — nothing else may be held up for it.
            synchronized(this@TaskReminders) {
            val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
            // Nags the receiver already owns. They are not re-armed here —
            // that would fire an extra alert on every refresh, which happens
            // after each task mutation — but they do compete for budget, or
            // the nag set would grow without bound and the cap would only
            // ever apply to new work. What it went overdue rides along as
            // the tiebreak: a nag's own fire time is always "in a few
            // seconds", so the due instant is the only thing left to rank
            // one against another.
            val carried = LinkedHashMap<String, Bid>()
            val wanted = LinkedHashMap<String, Bid>()
            var timed = 0
            for (t in tasks) {
                if (!t.dueDate.isIsoDate() || t.dueTime.isBlank()) continue
                if (t.dueDate > horizon) continue
                // One clock for every comparison below: the ranking, the
                // reminder points and the budget's own tie-break. Parsed
                // rather than read off the strings, because ClockTime takes
                // "9:00" as readily as "09:00" and a concatenated date and
                // time would sort those two the wrong way round.
                val dueAt = dueMillisOrNull(t.dueDate, t.dueTime) ?: continue
                val points = reminderPoints(
                    t.dueDate, t.dueTime, t.estimatedMinutes, nowMillis,
                )
                // Counted once the task has actually produced a point, so
                // the reported total is a denominator the numbers divide into
                // rather than one inflated by rows the parser rejected.
                if (points.isEmpty()) continue
                timed++
                for ((kind, at) in points) {
                    val key = alarmKey(t.id, kind)
                    val nag = kind == ReminderKind.Overdue
                    val bid = Bid(
                        key = key,
                        nag = nag,
                        fire = if (nag) nowMillis + NAG_FIRE_OFFSET else at,
                        // The moment it went overdue, for the nag ordering
                        // below. Zero for one-shots, which order by fire time
                        // alone and never tie.
                        tie = if (nag) dueAt else 0L,
                    )
                    // An armed nag owns its own cadence: re-arming one here
                    // would fire an extra alert on every refresh, so it is
                    // carried — but it still competes for budget.
                    if (nag && key in armed) carried[key] = bid else wanted[key] = bid
                }
            }
            // The budget (#165). One-shots rank ahead of nags, each group
            // nearest-first: when the budget runs out, the right thing to
            // lose is a repeating nag, not an alert the user has no other
            // way of learning is coming. Ranking purely by fire time — as
            // this did — meant a long-dated task could take every kind of
            // alert away from a task that was due tomorrow. Nags tie on fire
            // time, so they are ordered by how long each has been overdue.
            val candidates = ArrayList<Bid>(wanted.size + carried.size)
            candidates.addAll(wanted.values)
            candidates.addAll(carried.values)
            val capped = candidates
                .sortedWith(compareBy({ it.nag }, { it.fire }, { it.tie }))
                .take(MAX_ALARMS)
                .map { it.key }
                .toSet()
            val keep = LinkedHashSet(capped)
            for (key in armed) {
                if (key in keep) continue
                // Keys from 4.4 and earlier were a bare task id, armed with
                // a different PendingIntent identity (no "/<kind>" in the
                // data URI) — cancel it that way or it would fire once,
                // after the upgrade, for the old "due now" moment.
                if (key.contains('#')) cancelKind(appCtx, key) else cancelLegacy(appCtx, key)
            }
            for (bid in wanted.values) {
                if (bid.key in capped) armAlarm(appCtx, bid.key, bid.fire)
            }
            val byKey = candidates.associateBy { it.key }
            val kept = capped.mapNotNull(byKey::get)
            val wantedOneShots = candidates.count { !it.nag }
            // Armed set and the budget that describes it go in together: a
            // crash between two applies would leave the reported numbers
            // describing an alarm set that is no longer the live one.
            prefs(appCtx).edit()
                .putStringSet(PREF_ARMED, keep)
                .putString(
                    PREF_BUDGET,
                    Budget(
                        tasks = timed,
                        wanted = candidates.size,
                        cap = MAX_ALARMS,
                        armed = capped.size,
                        droppedOneShots = wantedOneShots - kept.count { !it.nag },
                        droppedNags = (candidates.size - wantedOneShots) -
                            kept.count { it.nag },
                    ).encode(),
                )
                .apply()
            }
            RefreshOutcome.Ok
        }
    }

    /**
     * Drop every reminder for one task (all kinds). Like [rearmOverdue] this
     * leaves PREF_BUDGET alone: the budget is a snapshot of the last
     * [refresh], and one task's alarms coming or going does not make that
     * snapshot less true.
     */
    @Synchronized
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
    @Synchronized
    internal fun rearmOverdue(context: Context, taskId: String) {
        // Arms outside the cap and does not touch PREF_BUDGET: killing a
        // live nag mid-cycle to honour a budget is worse than briefly
        // exceeding one. The reported budget is therefore exact only after
        // the next [refresh], which re-derives both from the same pass.
        //
        // Synchronised on the object, like every other writer of PREF_ARMED:
        // this is a read-modify-write against a set [refresh] replaces
        // wholesale, so a nag firing mid-refresh must not have its key
        // overwritten by that refresh finishing a moment later.
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
    @Synchronized
    private fun cancelKind(appCtx: Context, key: String) {
        val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        mgr.cancel(operation(appCtx, key))
        val armed = prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()
        if (key in armed) {
            prefs(appCtx).edit().putStringSet(PREF_ARMED, armed - key).apply()
        }
    }

    /** Cancel a pre-4.5 alarm, whose identity had no kind segment. */
    @Synchronized
    private fun cancelLegacy(appCtx: Context, taskId: String) {
        val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        val fire = Intent(appCtx, TaskAlarmReceiver::class.java)
            .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
            .setData(Uri.parse("agento://reminder/${Uri.encode(taskId)}"))
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

    /**
     * What the last [refresh] had to choose between (#165).
     *
     * [droppedOneShots] is the number that matters: a missed start or due
     * alert is invisible, because nothing anywhere says "no alarm was set
     * for this". A dropped nag costs the user nothing they can see, since
     * the task is still listed, still in the widget and still marked
     * overdue. Both counts are surfaced rather than swallowed, because a
     * silently truncated budget is indistinguishable from a bug.
     */
    internal data class Budget(
        val tasks: Int,
        val wanted: Int,
        val cap: Int,
        val armed: Int,
        val droppedOneShots: Int,
        val droppedNags: Int,
    ) {
        val dropped: Int get() = droppedOneShots + droppedNags

        /** One line, for the diagnostics card and the exported report. */
        fun line(): String {
            // Only the non-zero counts: "dropped 0 alert(s) and 3 nag(s)"
            // reads as a bug in the counter rather than as a fact.
            val lost = buildList {
                if (droppedOneShots > 0) add("$droppedOneShots alert(s)")
                if (droppedNags > 0) add("$droppedNags nag(s)")
            }
            val dropped = if (lost.isEmpty()) {
                ""
            } else {
                " — OVER BUDGET, dropped " + lost.joinToString(" and ")
            }
            return "reminders $armed/$cap wanted=$wanted tasks=$tasks$dropped"
        }
    }

    /**
     * One alarm's claim on the budget, and the order it claims in.
     *
     * [tie] is the parsed due instant, used to order nags against each other
     * once they all share [NAG_FIRE_OFFSET]. Longest overdue wins the slot.
     */
    private data class Bid(
        val key: String,
        val nag: Boolean,
        val fire: Long,
        val tie: Long,
    )

    /**
     * What the last refresh decided, or null before the first one.
     *
     * Six small non-negative counts in one comma-separated pref rather than
     * six keys or a joined object: [SharedPreferences] has no array accessor
     * (that is `Bundle`), and a single key means one atomic write and one
     * read, with no partial state to guard against.
     */
    internal fun budget(appCtx: Context): Budget? {
        val raw = prefs(appCtx).getString(PREF_BUDGET, null) ?: return null
        val parts = raw.split(',').map { it.toIntOrNull() ?: return null }
        // Counts are never negative: anything else is a corrupt or
        // hand-edited value, and reporting "dropped -1 alert(s)" would be
        // worse than reporting nothing.
        return if (parts.size == 6 && parts.all { it >= 0 }) {
            Budget(parts[0], parts[1], parts[2], parts[3], parts[4], parts[5])
        } else {
            null
        }
    }

    /** The six counts, as one pref value. */
    private fun Budget.encode(): String =
        "$tasks,$wanted,$cap,$armed,$droppedOneShots,$droppedNags"

    /** Drop every armed alarm (reminders disabled). Legacy bare-id keys
     * need the old identity, exactly as in refresh — otherwise a
     * pre-4.5 alarm survives the switch-off and fires afterwards. */
    @Synchronized
    fun cancelAll(context: Context) {
        val appCtx = context.applicationContext
        for (key in prefs(appCtx).getStringSet(PREF_ARMED, emptySet()).orEmpty()) {
            if (key.contains('#')) cancelKind(appCtx, key) else cancelLegacy(appCtx, key)
        }
        // The budget goes with them: leaving it behind would have Settings
        // reporting slots "used" against an alarm set nothing is holding.
        prefs(appCtx).edit().remove(PREF_ARMED).remove(PREF_BUDGET).apply()
    }

    private fun operation(appCtx: Context, key: String): PendingIntent {
        // Split on the LAST '#': the kind is a fixed suffix, so an id that
        // itself contained one would otherwise truncate the task id. Ids are
        // hex ObjectIds today, so this is belt-and-braces.
        val cut = key.lastIndexOf('#')
        val taskId = if (cut < 0) key else key.substring(0, cut)
        val kind = if (cut < 0) ReminderKind.Due.key else key.substring(cut + 1)
        // Distinct data URI per key: the PendingIntent stays unique even
        // if two keys ever share a hashCode.
        val fire = Intent(appCtx, TaskAlarmReceiver::class.java)
            .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
            .putExtra(EXTRA_REMINDER_KIND, kind)
            // The path segment is encoded: a '/' in an id would otherwise
            // change the URI's structure and break PendingIntent identity.
            .setData(Uri.parse("agento://reminder/${Uri.encode(taskId)}/$kind"))
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
        // An alarm with no recognisable kind — an intent from an older
        // build, say — is treated as the due-time alert: one notification
        // at the deadline, never a nag loop.
        val kind = reminderKindOrNull(intent.getStringExtra(EXTRA_REMINDER_KIND))
            ?: ReminderKind.Due
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
        val today = LocalDate.now(IST)
        val dueLine = if (task != null) {
            friendlyDue(task.dueDate, task.dueTime, today)
        } else {
            ""
        }
        // A zero estimate has no start of its own, so the start line would
        // repeat the due line. Compared as instants, not as rendered text:
        // the start is zero-padded and the due side is echoed from storage,
        // so equal moments can compare unequal as strings.
        val startIsDue = task != null && task.startMillisOrNull() == task.dueMillisOrNull()
        val startLine = if (startIsDue) {
            null
        } else {
            task?.startParts()?.let { (d, t) -> friendlyDue(d, t, today) }
                ?.takeIf { it.isNotEmpty() }
        }
        val message = reminderMessage(kind, dueLine, startLine)
        // Locals: task is nullable and conditions below don't smart-cast.
        val taskDesc = task?.description.orEmpty()
        // Expanded view, at most three lines: the message, the task's own
        // description, and the window it has to happen in. Deliberately not
        // a dump of every field — the repeat rule, the parallel flag and
        // the estimate are things to look up in the app, not to read on a
        // lock screen (issue #160).
        val big = buildList {
            add(message)
            if (taskDesc.isNotEmpty()) add(taskDesc)
            when {
                startLine != null && dueLine.isNotEmpty() -> add("$startLine \u2192 $dueLine")
                dueLine.isNotEmpty() -> add("Due $dueLine")
            }
        }.joinToString("\n")
        // Inline complete reuses the widget trampoline (no visible UI:
        // completes, refreshes widget/alarms, finishes). Data URI + own
        // request code: extras don't count for PendingIntent identity,
        // so without these one task's Done could complete another.
        val done = PendingIntent.getActivity(
            appCtx, ("done:$taskId").hashCode(),
            Intent(appCtx, TaskCompleteActivity::class.java)
                .putExtra(TaskWidget.EXTRA_COMPLETE_ID, taskId)
                .setData(Uri.parse("agento://reminder/${Uri.encode(taskId)}/complete")),
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
            .setData(Uri.parse("agento://reminder/${Uri.encode(taskId)}"))
        val tap = PendingIntent.getActivity(
            appCtx, taskId.hashCode(), launch,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notif = NotificationCompat.Builder(appCtx, TaskReminders.CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_menu_agenda)
            .setContentTitle(name)
            .setContentText(message)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setAutoCancel(true)
            .setContentIntent(tap)
            // No icon (0): framework checkables render badly as action
            // icons on some OEMs.
            .addAction(0, "Done", done)
        // Expanded view whenever it would add something: a task with no
        // description still carries its window, and "Due now" alone leaves
        // the time unsaid. When the body *is* just the message there is
        // no second page worth opening.
        if (big.lineSequence().count() > 1) {
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
