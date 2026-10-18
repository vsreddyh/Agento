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
    /** Optional voice phrase for TTS reminders (#306); blank falls back to [name]. */
    val speakName: String = "",
    val description: String = "",
    val dueDate: String = "",
    val dueTime: String = "",
    val estimatedMinutes: String = "",
    val repeatEvery: String = "",
    val repeatUnit: String = "days",
    val repeatDays: List<String> = emptyList(),
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
    speakName = speakName,
    description = description,
    dueDate = dueDate,
    dueTime = dueTime,
    estimatedMinutes = estimatedMinutes.toString(),
    repeatEvery = if (repeatEvery > 0) repeatEvery.toString() else "",
    repeatUnit = repeatUnit.ifEmpty { "days" },
    repeatDays = repeatDays,
    repeatCustom = repeatCustom,
    repeatRule = repeatRule,
    parallelable = parallelable,
    revision = revision,
)

/** Repeat units offered by the editor, matching the server's vocabulary. */
internal val REPEAT_UNITS = listOf("days", "weeks", "months", "years")

/** The weekdays the editor's "Select days" chips offer, Monday first —
 * matching the server's vocabulary (RepeatDays) and the order it stores a
 * selection in, so what the app shows and what the server stores agree. */
internal val REPEAT_DAYS = listOf("mon", "tue", "wed", "thu", "fri", "sat", "sun")

/** Bounds of the structured count, kept in step with the server. */
internal const val REPEAT_EVERY_MIN = 1
internal const val REPEAT_EVERY_MAX = 28

/** The chip label for one weekday value: "mon" -> "Mon". The server's
 * DayLabel, mirrored — an unknown value is shown as it arrived rather than
 * hidden, so a selection the app cannot name is still visible. */
internal fun dayLabel(day: String): String =
    day.replaceFirstChar { it.uppercase(Locale.ROOT) }

/**
 * Canonicalises a weekday selection the way the server does: trims, lowercases,
 * drops duplicates, and sorts into calendar order.
 *
 * Unknown values are DROPPED here, unlike server-side — the chips can only
 * produce known ones, so anything else came from a hand-edited response, and
 * sending it back would earn a 422. The round-trip compare cannot report that
 * drop — it canonicalises both sides, so a dropped token is already gone from
 * each before they are compared — and the server's 422 is what surfaces it on
 * the next write instead.
 */
internal fun canonicalRepeatDays(days: List<String>): List<String> {
    val kept = days.map { it.trim().lowercase(Locale.ROOT) }.toSet()
    return REPEAT_DAYS.filter { it in kept }
}

/**
 * The server's selected-days rule (#304): the due date the user picked is the
 * FIRST occurrence, so it has to be one of the days.
 *
 * Mirrored here so the form cannot enable Save on a write the server rejects —
 * the same contract [repeatOrNull] keeps for the count and the custom words.
 * A date that will not parse is false, not an exception: a half-picked form is
 * an ordinary state to be in, not a fault.
 */
internal fun dueDateIsSelectedDay(dueDate: String, days: List<String>): Boolean {
    if (days.isEmpty()) return true
    val date = runCatching { java.time.LocalDate.parse(dueDate.trim()) }.getOrNull()
        ?: return false
    // DayOfWeek is MONDAY = 1, and REPEAT_DAYS starts at Monday.
    return REPEAT_DAYS[date.dayOfWeek.value - 1] in canonicalRepeatDays(days)
}

/**
 * The draft's recurrence in the server's shape: an interval cadence, a
 * selected-days cadence, or a custom condition. [RepeatDraft.oneShot] is the
 * all-zero case, and null means the draft is inconsistent — a custom condition
 * with no words, a count outside 1-28, a half-typed number, or a selection that
 * canonicalises to nothing. Save stays disabled until it parses.
 *
 * One shape at a time by construction, which is what the server requires: the
 * dialog clears the other modes as it enters one (see ServerTaskDialog), so a
 * user cannot describe two cadences and have the server pick one of them.
 */
internal data class RepeatDraft(
    val every: Int = 0,
    val unit: String = "",
    val days: List<String> = emptyList(),
    val custom: Boolean = false,
    val text: String = "",
) {
    /** True when nothing repeats: the one-shot case. */
    val oneShot: Boolean get() = every == 0 && unit.isEmpty() && days.isEmpty() && !custom && text.isEmpty()
}

internal fun ServerTaskDraft.repeatOrNull(): RepeatDraft? {
    if (repeatCustom) {
        val text = repeatRule.trim()
        return if (text.isEmpty()) null else RepeatDraft(custom = true, text = text)
    }
    if (repeatDays.isNotEmpty()) {
        val days = canonicalRepeatDays(repeatDays)
        if (days.isEmpty()) return null
        return RepeatDraft(days = days)
    }
    val typed = repeatEvery.trim()
    if (typed.isEmpty()) {
        // No cadence: one-shot.
        return RepeatDraft()
    }
    val every = typed.toIntOrNull() ?: return null
    if (every < REPEAT_EVERY_MIN || every > REPEAT_EVERY_MAX) return null
    if (repeatUnit !in REPEAT_UNITS) return null
    return RepeatDraft(every = every, unit = repeatUnit)
}

/**
 * The task response contract this app speaks (#185).
 *
 * Every task response carries it as `contract_version`, and every write
 * asserts it alongside the fields it sent. Bump it in the same change as the
 * server's `TaskContractVersion` whenever the contract changes — a renamed
 * key, a new shape, a different rollover envelope — so a stale install on
 * either side reads as a version disagreement instead of silent wrong data.
 *
 * 2 — the `repeat_days` selection (#304). A new field, and an old app reading
 * a selected-days task as a one-shot is the same silent wrong data this number
 * exists to make loud.
 */
internal const val TASK_CONTRACT_VERSION = 3

/** Prefs key for the highest task contract this install has spoken (#185). */
internal const val CONTRACT_VERSION_SEEN_KEY = "contract_version_seen"

/**
 * Every key `TasksApi.create` sends (#185).
 *
 * The body is BUILT from this (one map, iterated in order), so the list
 * cannot drift from what is actually sent — and the CI drift step asserts
 * this set equals the server's `taskCreateFields`, so neither side can add,
 * rename or drop a field without the other noticing. That is the mechanical
 * form of the "kept key-for-key" mirror the Go round-trip test used to rely
 * on prose alone to maintain.
 */
internal val TASK_CREATE_KEYS = listOf(
    "name",
    "speak_name",
    "description",
    "due_date",
    "due_time",
    "estimated_minutes",
    "parallelable",
    "repeat_every",
    "repeat_unit",
    "repeat_days",
    "repeat_custom",
    "repeat_rule",
)

/**
 * Reads a contract version out of a decoded JSON value (#185).
 *
 * Lenient on the TYPE, strict on the meaning: int, long and double all
 * arrive as [Number], and a numeric string reads as its number — but
 * anything else (absent, null, `"latest"`) is 0, "predates versions",
 * which the write paths report rather than silently default past. A string
 * `"1"` warning as an old server would be a false positive of exactly the
 * class this check exists to remove.
 */
internal fun parseContractVersion(raw: Any?): Int = when (raw) {
    is Number -> raw.toInt()
    // A double-encoded "1.0" gets the same leniency Numbers do (3.0 -> 3):
    // without the toDouble fallback it warns as a pre-version server.
    is String -> raw.trim().toIntOrNull()
        ?: raw.trim().toDoubleOrNull()?.toInt()
        ?: 0
    else -> 0
    // Negatives fit neither bucket (0 = pre-version, >0 = a real version),
    // so they clamp to 0 rather than passing through as a version that can
    // never equal TASK_CONTRACT_VERSION yet reads as one.
}.coerceAtLeast(0)

/**
 * Whether a complete response disagrees on either half (#185 review).
 *
 * `next` is absent (null) for one-shots with nothing to roll over — nothing
 * to assert then. Otherwise each doc is asserted on its own: collapsing the
 * two with maxOf hides a half-disagreement in BOTH directions (task=1 +
 * pre-version next=0, and task=0 + next=1, both read as 1), and next is the
 * half that already broke once (#180).
 */
internal fun completeVersionMismatch(taskVersion: Int, nextVersion: Int?): Boolean =
    taskVersion != TASK_CONTRACT_VERSION ||
        (nextVersion != null && nextVersion != TASK_CONTRACT_VERSION)

/**
 * The highest contract in a response batch, or 0 when no task spoke one.
 *
 * One number per RESPONSE, not per row: `list` parses up to 200 tasks, and
 * a prefs read-modify-write per row is 200 I/Os per refresh for a single
 * fact. The callers record this once per batch instead.
 */
internal fun maxContractVersion(tasks: List<ServerTask>): Int =
    tasks.maxOfOrNull { it.contractVersion } ?: 0

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
    val speakName: String? = null,
    val description: String? = null,
    val dueDate: String? = null,
    val dueTime: String? = null,
    val estimatedMinutes: Int? = null,
    val repeatEvery: Int? = null,
    val repeatUnit: String? = null,
    val repeatDays: List<String>? = null,
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
    if (sent.speakName != null && sent.speakName.trim() != got.speakName.trim()) out += "speak_name"
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
    // Compared as canonical SELECTIONS, not as lists — the trim-insensitive rule
    // above, extended to a field where "the same value" means the same days in
    // any order or case. Both sides canonicalise (the app before sending, the
    // server before storing), so a difference that survives that is real.
    if (sent.repeatDays != null &&
        canonicalRepeatDays(sent.repeatDays) != canonicalRepeatDays(got.repeatDays)
    ) {
        out += "repeat_days"
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
