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
    // The revision the editor read. Never shown, never edited: it goes
    // straight back on save as expected_revision, so a task that is no
    // longer in the list (filter, truncation) still guards correctly.
    val revision: Int = 0,
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
    revision = revision,
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

/**
 * The task response contract this app speaks (#185).
 *
 * Every task response carries it as `contract_version`, and every write
 * asserts it alongside the fields it sent. Bump it in the same change as the
 * server's `TaskContractVersion` whenever the contract changes — a renamed
 * key, a new shape, a different rollover envelope — so a stale install on
 * either side reads as a version disagreement instead of silent wrong data.
 */
internal const val TASK_CONTRACT_VERSION = 1

/**
 * The task fields the app asserts after a write (#185).
 *
 * Every field is **nullable, and null means "not asserted"** — not "absent from the
 * server" and not "zero". That distinction is the whole design:
 *
 * - A `create` asserts every field, so nothing is null.
 * - A partial `update` asserts only what it sent. A field the app never sent is
 *   UNKNOWN, not unchanged, and asserting it would report a mismatch on every edit.
 *
 * The earlier shape of this built the expected value from the response and then copied
 * the sent fields over it. That compares trivially for the unsent fields, which is
 * correct but fragile: a field added to one side and not the other is silently
 * "asserted" as whatever the server said. Nulls make the assertion explicit at the
 * call site instead of implied by a merge.
 *
 * Zero and empty string are REAL values and are compared as such —
 * `estimated_minutes: 0` means "no estimate", and a check written on falsy defaults
 * reports it missing on every one-shot task, so it fires constantly and gets ignored.
 */
internal data class TaskContractSent(
    val name: String? = null,
    val description: String? = null,
    val dueDate: String? = null,
    val dueTime: String? = null,
    val estimatedMinutes: Int? = null,
    val repeatEvery: Int? = null,
    val repeatUnit: String? = null,
    val repeatCustom: Boolean? = null,
    val repeatRule: String? = null,
    val parallelable: Boolean? = null,
)

/**
 * Names the task fields the server stored that differ from what the app asked it to
 * store. Empty means the round trip agreed.
 *
 * #185: every field used to be read with a default, so a response from an older server
 * — or one where a field failed to persist — was indistinguishable from a genuine zero.
 * After the 4.6.0 structured-repeat change that meant an un-updated client read every
 * structured cadence as a one-shot and rendered the task with no repeat at all, with
 * nothing warning. During the 4.7.0 review the complete-response shape changed and the
 * old client read `nextDueDate = ""`, decided the task had not rolled over, and opened
 * a recreate draft asking for a date that already existed. Silent, and plausible enough
 * to look like a bug in the rollover rather than a disagreement between versions.
 *
 * Text is compared TRIMMED, because the app trims before sending and the server trims
 * before storing: a padded value that agrees after trimming IS agreement, and flagging
 * it would teach people to ignore this warning, defeating the mechanism.
 *
 * A null in [sent] skips that field — see [TaskContractSent].
 */
internal fun contractMismatches(
    sent: TaskContractSent,
    got: ServerTask,
    expectedId: String? = null,
): List<String> {
    val out = mutableListOf<String>()
    // Identity, two ways — an id is the one field with no sensible default.
    //
    //   - BLANK: `parseTask` already rejects those, so `parseOne` throws before this
    //     runs. Defence in depth, kept because `contractMismatches` is also called
    //     directly, and a comparator that skipped the one field making a record
    //     unusable would be the wrong default.
    //   - WRONG: a server answering with a different, perfectly valid record. Nothing
    //     else here notices — every field can match and the app stores a result for a
    //     task it never touched. `update` passes the id it asked to change; `create`
    //     passes none, because the server assigns it.
    // `else if`, not two `if`s: a blank `got.id` with a non-blank `expectedId` also fails
    // the comparison, and two arms reported "id" twice — the message read "id, id".
    // Unreachable through the wired path (parseOne throws on a blank id first) but the
    // comparator is also called directly, and it should not depend on its caller.
    if (got.id.isBlank()) {
        out += "id"
    } else if (expectedId != null && expectedId.trim().isNotEmpty() &&
        expectedId.trim() != got.id.trim()
    ) {
        out += "id"
    }
    // The contract itself, before any field (#185). 0 means the server
    // predates versions entirely; anything above TASK_CONTRACT_VERSION means
    // the server is newer than the app. Either way the fields below may have
    // been read with defaults — a missing repeat_custom defaulting to false
    // is exactly the 4.6.0 silent case — so the version disagreeing is
    // reported like any other disagreement, through the same warn-not-throw
    // path. The field name is what the server actually sent (or didn't),
    // which is what a reader needs to find it in the response.
    if (got.contractVersion != TASK_CONTRACT_VERSION) out += "contract_version"
    if (sent.name != null && sent.name.trim() != got.name.trim()) out += "name"
    if (sent.description != null &&
        sent.description.trim() != got.description.trim()
    ) out += "description"
    if (sent.dueDate != null && sent.dueDate.trim() != got.dueDate.trim()) out += "due_date"
    if (sent.dueTime != null && sent.dueTime.trim() != got.dueTime.trim()) out += "due_time"
    if (sent.estimatedMinutes != null && sent.estimatedMinutes != got.estimatedMinutes) {
        out += "estimated_minutes"
    }
    if (sent.repeatEvery != null && sent.repeatEvery != got.repeatEvery) {
        out += "repeat_every"
    }
    if (sent.repeatUnit != null && sent.repeatUnit.trim() != got.repeatUnit.trim()) {
        out += "repeat_unit"
    }
    if (sent.repeatCustom != null && sent.repeatCustom != got.repeatCustom) {
        out += "repeat_custom"
    }
    if (sent.repeatRule != null && sent.repeatRule.trim() != got.repeatRule.trim()) {
        out += "repeat_rule"
    }
    if (sent.parallelable != null && sent.parallelable != got.parallelable) {
        out += "parallelable"
    }
    return out
}
