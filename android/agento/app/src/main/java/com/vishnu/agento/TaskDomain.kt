package com.vishnu.agento

import java.util.Locale

/**
 * Task domain: the pure rules the Task Manager list is built from (#163).
 *
 * No Compose here on purpose. Bucketing, sorting, the start rule's callers
 * and the editor draft's validation are all plain functions over
 * [ServerTask], which is what makes them movable — and, once #162 lands,
 * testable — without dragging the UI along. Moved verbatim out of
 * MainActivity.kt; the only change is visibility (`private` to `internal`,
 * same package, so every caller keeps working).
 */

/** Server task states for the Task Manager filter (match `GET /api/tasks`). */
internal enum class ServerTaskFilter(val state: String, val title: String) {
    Open("open", "Open"),
    Done("done", "Done"),
    All("all", "All"),
}

/** Client-side sort for the task list (server returns one state at a time,
 * unsorted). Due puts undated tasks last; Created is newest first. */
internal enum class ServerTaskSort(val title: String) {
    Start("Start time"),
    Name("Name"),
    Created("Newest"),
    Estimate("Estimate"),
}

/**
 * Sort key for "when should this begin": the start moment, with the due
 * time as a tie-break so two tasks starting together still order by their
 * deadlines. Tasks with no usable due time sort last instead of first —
 * an undated task is not the most urgent thing you have.
 */
internal fun ServerTask.startSortKey(): Long =
    startMillisOrNull() ?: Long.MAX_VALUE

/**
 * Overdue means the moment has passed, not just the date: a task due at
 * 09:00 today at 14:00 is overdue, and the reminder engine already nags it
 * every 15 minutes. Matching that here keeps the list's red rows and its
 * Overdue group telling the same story. ISO YYYY-MM-DD compares
 * lexicographically; blank or malformed dates never count.
 *
 * [now] is passed in rather than read here so a row's colour and the group
 * it sits in are decided from the same instant — at a bucket boundary two
 * rows must not land on opposite sides of it.
 */
internal fun ServerTask.isOverdue(
    today: String,
    now: java.time.LocalDateTime,
): Boolean {
    if (!isOpen() || !dueDate.isIsoDate()) return false
    if (dueDate < today) return true
    if (dueDate != today) return false
    // Compared in millis, not via whole minutes: a task 30 seconds past due
    // is overdue now, and must match the nag that is already firing.
    val at = dueMillisOrNull(dueDate, dueTime) ?: return false
    return at <= now.atZone(IST).toInstant().toEpochMilli()
}

/**
 * Buckets for the grouped task list, in display order.
 *
 * Today is split by how long is left rather than shown as one "Today" pile:
 * what people actually ask about a task is "how soon?", and the six
 * horizons below answer it without opening anything. They stop at the end
 * of today on purpose — a task due in three hours is not "tomorrow", and
 * pretending otherwise hides it. Days from tomorrow on keep plain day
 * groups, where a time-of-day split adds nothing.
 *
 * Done tasks in the All view collect in Completed at the bottom.
 */
internal enum class DueBucket(val title: String) {
    Overdue("Overdue"),
    Current("Current"),
    ThisHour("This hour"),
    NextHour("Next hour"),
    In2To3Hours("Next 2-3 hours"),
    In3To6Hours("Next 3-6 hours"),
    In6To12Hours("Next 6-12 hours"),
    LaterToday("Later today"),
    Tomorrow("Tomorrow"),
    ThisWeek("This week"),
    Later("Later"),
    NoDate("No due date"),
    Completed("Completed"),
}

/**
 * The group this task belongs to.
 *
 * Everything ahead of the deadline is measured to the task's **start**
 * time (`due - estimated_minutes`), not its due time, because the question
 * a list answers is "when should I begin?", and that is the moment the
 * "Start now" reminder fires. A task due in three hours with a one-hour
 * estimate belongs in the next hour or two, not three: it is what you
 * should be starting, not what you must finish by.
 *
 * Past the start time and short of the deadline is **Current** — the window
 * in which the work is meant to happen. Past the deadline it is
 * **Overdue**, which is also what the reminder engine calls it (it nags
 * every 15 minutes), so the list and the alerts never disagree.
 *
 * A today task with no time at all lands in Later today: nothing is known
 * about *when*, and "this hour" would be a guess.
 */
internal fun ServerTask.dueBucket(
    today: java.time.LocalDate,
    now: java.time.LocalDateTime,
): DueBucket {
    if (!isOpen()) return DueBucket.Completed
    if (!dueDate.isIsoDate()) return DueBucket.NoDate
    val s = dueDate
    val nowMillis = now.atZone(IST).toInstant().toEpochMilli()
    val dueAt = dueMillisOrNull()
    if (dueAt == null) {
        // A date with no time (rows predating mandatory due_time): the day
        // is known, so it keeps its day group rather than being reported as
        // undated. A today one has no idea *when*, so Later today is the
        // honest answer — "This hour" would be a guess.
        return when {
            s < today.toString() -> DueBucket.Overdue
            s == today.toString() -> DueBucket.LaterToday
            s == today.plusDays(1).toString() -> DueBucket.Tomorrow
            s <= today.plusDays(7).toString() -> DueBucket.ThisWeek
            else -> DueBucket.Later
        }
    }
    // One shared start rule (TasksApi.startMillisOrNull), so the list and
    // the reminder engine cannot drift apart on when a task "starts".
    val startAt = startMillisOrNull() ?: dueAt
    val startDay = java.time.Instant.ofEpochMilli(startAt)
        .atZone(IST).toLocalDate().toString()
    val t = today.toString()
    return when {
        // Past the deadline: overdue, which is also what the reminder
        // engine calls it. Compared in millis, so a task 30 seconds past due
        // is overdue now rather than up to a minute later.
        dueAt <= nowMillis -> DueBucket.Overdue
        // The start moment has arrived and the deadline has not: this is
        // what should be under way now.
        startAt <= nowMillis -> DueBucket.Current
        // Which day a task belongs to is the day it *starts* on, not the
        // day it is due: due tomorrow 00:30 with a one-hour estimate is
        // something to start tonight, and tonight is today.
        startDay == t -> {
            val left = startAt - nowMillis
            when {
                // Every name is the range it actually covers: this hour, the
                // hour after, 2-3h, 3-6h, 6-12h, and everything still left
                // today. No two names overlap, so a reader never has to
                // guess which bucket a row came from.
                left < 60L * 60_000 -> DueBucket.ThisHour
                left < 120L * 60_000 -> DueBucket.NextHour
                left < 180L * 60_000 -> DueBucket.In2To3Hours
                left < 360L * 60_000 -> DueBucket.In3To6Hours
                left < 720L * 60_000 -> DueBucket.In6To12Hours
                else -> DueBucket.LaterToday
            }
        }
        // Days from tomorrow on keep plain day groups, where a
        // time-of-day split adds nothing.
        startDay == today.plusDays(1).toString() -> DueBucket.Tomorrow
        startDay <= today.plusDays(7).toString() -> DueBucket.ThisWeek
        else -> DueBucket.Later
    }
}

internal fun List<ServerTask>.sortedByMode(mode: ServerTaskSort): List<ServerTask> =
    when (mode) {
        ServerTaskSort.Start -> sortedWith(
            compareBy<ServerTask> { it.startSortKey() }
                .thenBy({ it.dueMillisOrNull() ?: Long.MAX_VALUE })
                .thenBy({ it.name.lowercase(Locale.ROOT) }),
        )
        ServerTaskSort.Name -> sortedBy { it.name.lowercase(Locale.ROOT) }
        ServerTaskSort.Created -> sortedByDescending { it.createdAt }
        ServerTaskSort.Estimate -> sortedWith(
            compareByDescending<ServerTask> { it.estimatedMinutes > 0 }
                .thenByDescending { it.estimatedMinutes },
        )
    }

/** Editor draft for a server task (id empty = new). Text fields stay strings
 * so half-typed input (e.g. minutes, the repeat count) survives; parsed on
 * save. The repeat count is a string for the same reason. */
internal data class ServerTaskDraft(
    val id: String = "",
    val name: String = "",
    val description: String = "",
    val dueDate: String = "",
    val dueTime: String = "",
    val estimatedMinutes: String = "",
    val repeatEvery: String = "",
    val repeatUnit: String = "days",
    val repeatCustom: Boolean = false,
    val repeatRule: String = "",
    val parallelable: Boolean = false,
)

internal fun ServerTask.toDraft() = ServerTaskDraft(
    id = id,
    name = name,
    description = description,
    dueDate = dueDate,
    dueTime = dueTime,
    estimatedMinutes = estimatedMinutes.toString(),
    repeatEvery = if (repeatEvery > 0) repeatEvery.toString() else "",
    repeatUnit = repeatUnit.ifEmpty { "days" },
    repeatCustom = repeatCustom,
    repeatRule = repeatRule,
    parallelable = parallelable,
)

/** Repeat units offered by the editor, matching the server's vocabulary. */
internal val REPEAT_UNITS = listOf("days", "weeks", "months", "years")

/** Bounds of the structured count, kept in step with the server. */
internal const val REPEAT_EVERY_MIN = 1
internal const val REPEAT_EVERY_MAX = 28

/** The draft's recurrence as (every, unit, customText), or null when it is
 * inconsistent — a custom condition with no words, or a count outside
 * 1-28, or a half-typed number. Save stays disabled until it parses. */
internal fun ServerTaskDraft.repeatOrNull(): Triple<Int, String, String>? {
    if (repeatCustom) {
        val text = repeatRule.trim()
        return if (text.isEmpty()) null else Triple(0, "", text)
    }
    val typed = repeatEvery.trim()
    if (typed.isEmpty()) {
        // No cadence: one-shot.
        return Triple(0, "", "")
    }
    val every = typed.toIntOrNull() ?: return null
    if (every < REPEAT_EVERY_MIN || every > REPEAT_EVERY_MAX) return null
    if (repeatUnit !in REPEAT_UNITS) return null
    return Triple(every, repeatUnit, "")
}
